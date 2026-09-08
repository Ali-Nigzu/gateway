package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	artifactRegistryBase          = "https://artifactregistry.googleapis.com"
	artifactRegistryRepository    = "projects/camosbase/locations/europe-west2/repositories/camos-gateway-prod"
	artifactPackage               = "gateway"
	maximumGatewayArtifactSize    = int64(2 << 30)
	maximumArtifactMetadataBytes  = int64(4 << 20)
	maximumArtifactMetadataPages  = 32
	maximumArtifactMetadataFiles  = 4096
	maximumArtifactPageTokenSize  = 8192
	artifactMetadataTimeout       = 45 * time.Second
	artifactDownloadHeaderTimeout = 60 * time.Second
	artifactMeaningfulProgress    = int64(64 << 10)
)

var artifactRegistryEndpoint = artifactRegistryBase

type artifactFailureCategory string

const (
	artifactFailureMetadata          artifactFailureCategory = "metadata"
	artifactFailureContract          artifactFailureCategory = "artifact-contract"
	artifactFailureDownload          artifactFailureCategory = "download"
	artifactFailureStall             artifactFailureCategory = "stall"
	artifactFailureSize              artifactFailureCategory = "size"
	artifactFailureSHA               artifactFailureCategory = "sha"
	artifactFailureCandidateIdentity artifactFailureCategory = "candidate-identity"
)

type artifactLifecycleError struct {
	Category artifactFailureCategory
	Err      error
}

func (failure *artifactLifecycleError) Error() string { return failure.Err.Error() }
func (failure *artifactLifecycleError) Unwrap() error { return failure.Err }

func newArtifactFailure(category artifactFailureCategory, message string) error {
	return &artifactLifecycleError{Category: category, Err: errors.New(message)}
}

func wrapArtifactFailure(category artifactFailureCategory, message string, err error) error {
	return &artifactLifecycleError{Category: category, Err: fmt.Errorf("%s: %w", message, err)}
}

func artifactFailureCategoryOf(err error) artifactFailureCategory {
	var failure *artifactLifecycleError
	if errors.As(err, &failure) {
		return failure.Category
	}
	return artifactFailureDownload
}

type artifactTransferPolicy struct {
	StallTimeout    time.Duration
	AbsoluteTimeout time.Duration
	CheckInterval   time.Duration
}

var gatewayArtifactTransferPolicy = artifactTransferPolicy{
	StallTimeout:    2 * time.Minute,
	AbsoluteTimeout: 2 * time.Hour,
	CheckInterval:   5 * time.Second,
}

type artifactHash struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

type artifactFile struct {
	Name      string         `json:"name"`
	SizeBytes string         `json:"sizeBytes"`
	Hashes    []artifactHash `json:"hashes"`
	Owner     string         `json:"owner"`
}

type artifactFileList struct {
	Files         []artifactFile `json:"files"`
	NextPageToken string         `json:"nextPageToken"`
}

type genericArtifactFileID struct {
	Package  string
	Version  string
	Filename string
}

func artifactPlatformFilename(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "windows/amd64":
		return "windows-amd64.exe", nil
	case "darwin/arm64":
		return "darwin-arm64", nil
	case "darwin/amd64":
		return "darwin-amd64", nil
	case "linux/amd64":
		return "linux-amd64", nil
	default:
		return "", fmt.Errorf("unsupported Gateway platform %s/%s", goos, goarch)
	}
}

func validateArtifactVersion(version string) error {
	if version == "" || len(version) > 128 {
		return errors.New("Gateway artifact version is invalid")
	}
	isAlphaNumeric := func(character byte) bool {
		return (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9')
	}
	// Artifact Registry version IDs are lowercase and must start/end with an
	// alphanumeric character. Deliberately exclude its optional ':' character:
	// Generic file IDs use package:version:filename and this appliance keeps
	// that externally visible identity unambiguous.
	if !isAlphaNumeric(version[0]) || !isAlphaNumeric(version[len(version)-1]) {
		return errors.New("Gateway artifact version is invalid")
	}
	for _, character := range version {
		if (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune(".-+~", character) {
			continue
		}
		return errors.New("Gateway artifact version is invalid")
	}
	return nil
}

func artifactVersionOwner(version string) string {
	return fmt.Sprintf("%s/packages/%s/versions/%s", artifactRegistryRepository, artifactPackage, version)
}

// artifactFileID parses Google's Generic Artifact Registry identity. A real
// resource is named .../files/gateway:1.0:windows-amd64.exe; the file ID is not
// just the platform filename.
func artifactFileID(name string) (genericArtifactFileID, bool) {
	prefix := artifactRegistryRepository + "/files/"
	if !strings.HasPrefix(name, prefix) {
		return genericArtifactFileID{}, false
	}
	escapedID := strings.TrimPrefix(name, prefix)
	if escapedID == "" || strings.Contains(escapedID, "/") {
		return genericArtifactFileID{}, false
	}
	decodedID, err := url.PathUnescape(escapedID)
	if err != nil || strings.ContainsAny(decodedID, "/\\") {
		return genericArtifactFileID{}, false
	}
	parts := strings.Split(decodedID, ":")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return genericArtifactFileID{}, false
	}
	return genericArtifactFileID{Package: parts[0], Version: parts[1], Filename: parts[2]}, true
}

func selectArtifactFile(files []artifactFile, version, filename string) (artifactFile, []byte, error) {
	if err := validateArtifactVersion(version); err != nil {
		return artifactFile{}, nil, wrapArtifactFailure(artifactFailureContract, "invalid requested version", err)
	}
	owner := artifactVersionOwner(version)
	var selected *artifactFile
	for index := range files {
		file := &files[index]
		identity, valid := artifactFileID(file.Name)
		if !valid || file.Owner != owner ||
			identity.Package != artifactPackage ||
			identity.Version != version ||
			identity.Filename != filename {
			continue
		}
		if selected != nil {
			return artifactFile{}, nil, newArtifactFailure(artifactFailureContract, "Artifact Registry returned duplicate exact files")
		}
		selected = file
	}
	if selected == nil {
		return artifactFile{}, nil, newArtifactFailure(artifactFailureContract, "requested Gateway artifact is missing")
	}
	if _, err := artifactExpectedSize(selected.SizeBytes); err != nil {
		return artifactFile{}, nil, newArtifactFailure(artifactFailureSize, "Gateway artifact size is invalid")
	}

	var selectedHash []byte
	shaCount := 0
	for _, hash := range selected.Hashes {
		if hash.Type != "SHA256" {
			continue
		}
		shaCount++
		value, err := base64.StdEncoding.Strict().DecodeString(hash.Value)
		if err != nil || len(value) != sha256.Size || base64.StdEncoding.EncodeToString(value) != hash.Value {
			return artifactFile{}, nil, newArtifactFailure(artifactFailureSHA, "Gateway artifact SHA-256 is invalid")
		}
		selectedHash = value
	}
	if shaCount == 0 {
		return artifactFile{}, nil, newArtifactFailure(artifactFailureSHA, "Gateway artifact SHA-256 is missing")
	}
	if shaCount != 1 {
		return artifactFile{}, nil, newArtifactFailure(artifactFailureSHA, "Gateway artifact SHA-256 is ambiguous")
	}
	return *selected, selectedHash, nil
}

func listArtifactVersion(ctx context.Context, client *http.Client, version string) ([]artifactFile, error) {
	if err := validateArtifactVersion(version); err != nil {
		return nil, wrapArtifactFailure(artifactFailureContract, "invalid requested version", err)
	}
	owner := artifactVersionOwner(version)
	endpoint := artifactRegistryEndpoint + "/v1/" + artifactRegistryRepository + "/files"
	var files []artifactFile
	pageToken := ""
	seenPageTokens := make(map[string]struct{})
	for pageNumber := 0; pageNumber < maximumArtifactMetadataPages; pageNumber++ {
		values := url.Values{
			"filter":   {fmt.Sprintf("owner=\"%s\"", owner)},
			"pageSize": {"1000"},
		}
		if pageToken != "" {
			values.Set("pageToken", pageToken)
		}
		requestCtx, cancel := context.WithTimeout(ctx, artifactMetadataTimeout)
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint+"?"+values.Encode(), nil)
		if err != nil {
			cancel()
			return nil, wrapArtifactFailure(artifactFailureMetadata, "metadata request creation failed", err)
		}
		metadataClient := *client
		metadataClient.Timeout = 0
		response, err := metadataClient.Do(request)
		if err != nil {
			cancel()
			return nil, newArtifactFailure(artifactFailureMetadata, "Artifact Registry metadata request failed")
		}
		page, decodeErr := decodeArtifactMetadata(response.Body)
		closeErr := response.Body.Close()
		cancel()
		if response.StatusCode != http.StatusOK {
			return nil, newArtifactFailure(artifactFailureMetadata, fmt.Sprintf("Artifact Registry metadata request failed with status %d", response.StatusCode))
		}
		if decodeErr != nil || closeErr != nil {
			return nil, newArtifactFailure(artifactFailureMetadata, "Artifact Registry metadata response is invalid")
		}
		if len(page.Files) > maximumArtifactMetadataFiles-len(files) {
			return nil, newArtifactFailure(artifactFailureMetadata, "Artifact Registry metadata contains too many files")
		}
		files = append(files, page.Files...)
		if page.NextPageToken == "" {
			return files, nil
		}
		if len(page.NextPageToken) > maximumArtifactPageTokenSize {
			return nil, newArtifactFailure(artifactFailureMetadata, "Artifact Registry metadata pagination is invalid")
		}
		if _, repeated := seenPageTokens[page.NextPageToken]; repeated {
			return nil, newArtifactFailure(artifactFailureMetadata, "Artifact Registry metadata pagination is invalid")
		}
		seenPageTokens[page.NextPageToken] = struct{}{}
		pageToken = page.NextPageToken
	}
	return nil, newArtifactFailure(artifactFailureMetadata, "Artifact Registry metadata contains too many pages")
}

func decodeArtifactMetadata(body io.Reader) (artifactFileList, error) {
	encoded, err := io.ReadAll(io.LimitReader(body, maximumArtifactMetadataBytes+1))
	if err != nil || int64(len(encoded)) > maximumArtifactMetadataBytes {
		return artifactFileList{}, errors.New("metadata body exceeds its bound")
	}
	trimmed := bytes.TrimSpace(encoded)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return artifactFileList{}, errors.New("metadata body is not a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	var page artifactFileList
	if err := decoder.Decode(&page); err != nil {
		return artifactFileList{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return artifactFileList{}, errors.New("metadata body contains trailing JSON")
	}
	return page, nil
}

// downloadGatewayCandidate holds the native download lock until its caller
// finishes the commit attempt. This keeps the deterministic candidate paths
// single-owner without holding the lifecycle lock across a potentially slow
// body transfer; terminal removal can still commit immediately.
func downloadGatewayCandidate(
	ctx context.Context,
	client *http.Client,
	desiredVersion string,
	candidatePath string,
) (func(), error) {
	filename, err := artifactPlatformFilename(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return nil, err
	}
	if desiredVersion == BuildVersion && filename == currentGatewayReleaseTarget &&
		currentGatewayReleaseStamp() != expectedGatewayReleaseStamp(desiredVersion, filename) {
		return nil, newArtifactFailure(artifactFailureCandidateIdentity, "running Gateway release stamp invariant failed")
	}
	release, err := acquireGatewayDownloadLock(lifecycleOperationLockWait)
	if err != nil {
		return nil, wrapArtifactFailure(artifactFailureDownload, "Gateway download lock failed", err)
	}
	if err := downloadGatewayCandidateForTarget(
		ctx,
		client,
		desiredVersion,
		candidatePath,
		filename,
		gatewayArtifactTransferPolicy,
		inspectGatewayCandidate,
	); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

type gatewayCandidateInspector func(string, string, string) (gatewayCandidateIdentity, error)

func downloadGatewayCandidateForTarget(
	ctx context.Context,
	client *http.Client,
	desiredVersion, candidatePath, filename string,
	policy artifactTransferPolicy,
	inspector gatewayCandidateInspector,
) (resultErr error) {
	if err := validateArtifactVersion(desiredVersion); err != nil {
		return wrapArtifactFailure(artifactFailureContract, "invalid requested version", err)
	}
	if _, err := artifactTargetForFilename(filename); err != nil {
		return wrapArtifactFailure(artifactFailureContract, "invalid target", err)
	}
	if err := validateArtifactTransferPolicy(policy); err != nil {
		return wrapArtifactFailure(artifactFailureDownload, "invalid transfer policy", err)
	}
	files, err := listArtifactVersion(ctx, client, desiredVersion)
	if err != nil {
		return err
	}
	file, expectedHash, err := selectArtifactFile(files, desiredVersion, filename)
	if err != nil {
		return err
	}
	expectedSize, err := artifactExpectedSize(file.SizeBytes)
	if err != nil {
		return newArtifactFailure(artifactFailureSize, "Gateway artifact size is invalid")
	}

	temporaryPath := candidatePath + ".downloading"
	if err := removeDisposableCandidate(temporaryPath); err != nil {
		return wrapArtifactFailure(artifactFailureDownload, "stale Gateway download cleanup failed", err)
	}
	if err := removeDisposableCandidate(candidatePath); err != nil {
		return wrapArtifactFailure(artifactFailureDownload, "stale Gateway candidate cleanup failed", err)
	}

	transferCtx, cancelTransfer := context.WithTimeout(ctx, policy.AbsoluteTimeout)
	defer cancelTransfer()
	request, err := http.NewRequestWithContext(transferCtx, http.MethodGet, artifactDownloadURL(file), nil)
	if err != nil {
		return wrapArtifactFailure(artifactFailureDownload, "download request creation failed", err)
	}
	response, err := doArtifactDownloadRequest(transferCtx, cancelTransfer, client, request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return newArtifactFailure(artifactFailureDownload, fmt.Sprintf("Gateway artifact download failed with status %d", response.StatusCode))
	}
	if response.ContentLength >= 0 && response.ContentLength != expectedSize {
		return newArtifactFailure(artifactFailureSize, "Gateway artifact Content-Length does not match metadata")
	}

	temporary, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return wrapArtifactFailure(artifactFailureDownload, "Gateway candidate creation failed", err)
	}
	temporaryOpen := true
	published := false
	defer func() {
		if temporaryOpen {
			if err := temporary.Close(); err != nil {
				resultErr = errors.Join(resultErr, wrapArtifactFailure(artifactFailureDownload, "Gateway candidate close failed", err))
			}
		}
		if !published {
			if err := removeDisposableCandidate(temporaryPath); err != nil {
				resultErr = errors.Join(resultErr, wrapArtifactFailure(artifactFailureDownload, "Gateway partial-download cleanup failed", err))
			}
		}
	}()

	hash := sha256.New()
	written, stalled, copyErr := copyArtifactWithProgress(
		transferCtx,
		cancelTransfer,
		io.MultiWriter(temporary, hash),
		response.Body,
		expectedSize+1,
		policy,
	)
	if stalled {
		return newArtifactFailure(artifactFailureStall, "Gateway artifact download stalled")
	}
	if copyErr != nil {
		if ctx.Err() != nil {
			return newArtifactFailure(artifactFailureDownload, "Gateway artifact download was cancelled")
		}
		if errors.Is(transferCtx.Err(), context.DeadlineExceeded) {
			return newArtifactFailure(artifactFailureDownload, "Gateway artifact download exceeded its absolute duration")
		}
		return newArtifactFailure(artifactFailureDownload, "Gateway artifact download was incomplete")
	}
	if written != expectedSize || written <= 0 || written > maximumGatewayArtifactSize {
		return newArtifactFailure(artifactFailureSize, "Gateway artifact size does not match metadata")
	}
	calculatedHash := hash.Sum(nil)
	if !bytes.Equal(calculatedHash, expectedHash) {
		return newArtifactFailure(artifactFailureSHA, "Gateway artifact SHA-256 mismatch")
	}
	if err := temporary.Sync(); err != nil {
		return wrapArtifactFailure(artifactFailureDownload, "Gateway candidate sync failed", err)
	}
	if err := temporary.Close(); err != nil {
		temporaryOpen = false
		return wrapArtifactFailure(artifactFailureDownload, "Gateway candidate close failed", err)
	}
	temporaryOpen = false
	if err := secureInstalledExecutable(temporaryPath); err != nil {
		return wrapArtifactFailure(artifactFailureDownload, "Gateway candidate permissions failed", err)
	}
	identity, err := inspector(temporaryPath, desiredVersion, filename)
	if err != nil {
		return wrapArtifactFailure(artifactFailureCandidateIdentity, "Gateway candidate identity failed", err)
	}
	if identity.Size != expectedSize || !bytes.Equal(identity.SHA256[:], expectedHash) {
		return newArtifactFailure(artifactFailureCandidateIdentity, "Gateway candidate changed during validation")
	}
	if err := replaceFileAtomically(temporaryPath, candidatePath); err != nil {
		return wrapArtifactFailure(artifactFailureDownload, "Gateway candidate publication failed", err)
	}
	published = true
	if err := syncParentDirectory(candidatePath); err != nil {
		return wrapArtifactFailure(artifactFailureDownload, "Gateway candidate directory sync failed", err)
	}
	return nil
}

func artifactDownloadURL(file artifactFile) string {
	return artifactRegistryEndpoint + "/download/v1/" + file.Name + ":download?alt=media"
}

type artifactHTTPResult struct {
	response *http.Response
	err      error
}

func doArtifactDownloadRequest(ctx context.Context, cancel context.CancelFunc, client *http.Client, request *http.Request) (*http.Response, error) {
	downloadClient := *client
	downloadClient.Timeout = 0
	// Keep this unbuffered so a response that loses the timeout/cancellation
	// race cannot be abandoned in a channel with an open response body.
	result := make(chan artifactHTTPResult)
	go func() {
		response, err := downloadClient.Do(request)
		select {
		case result <- artifactHTTPResult{response: response, err: err}:
		case <-ctx.Done():
			if response != nil {
				_ = response.Body.Close()
			}
		}
	}()
	timer := time.NewTimer(artifactDownloadHeaderTimeout)
	defer timer.Stop()
	select {
	case completed := <-result:
		if completed.err != nil {
			return nil, newArtifactFailure(artifactFailureDownload, "Gateway artifact download request failed")
		}
		return completed.response, nil
	case <-timer.C:
		cancel()
		return nil, newArtifactFailure(artifactFailureDownload, "Gateway artifact download response timed out")
	case <-ctx.Done():
		return nil, newArtifactFailure(artifactFailureDownload, "Gateway artifact download was cancelled")
	}
}

func validateArtifactTransferPolicy(policy artifactTransferPolicy) error {
	if policy.StallTimeout <= 0 || policy.AbsoluteTimeout <= 0 ||
		policy.CheckInterval <= 0 || policy.StallTimeout >= policy.AbsoluteTimeout ||
		policy.CheckInterval > policy.StallTimeout {
		return errors.New("Gateway artifact transfer policy is invalid")
	}
	return nil
}

type progressReader struct {
	reader       io.Reader
	lastProgress *atomic.Int64
	threshold    int64
	unreported   int64
}

func (reader *progressReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	if count > 0 {
		reader.unreported += int64(count)
		if reader.unreported >= reader.threshold {
			reader.lastProgress.Store(time.Now().UnixNano())
			reader.unreported = 0
		}
	}
	return count, err
}

func copyArtifactWithProgress(
	ctx context.Context,
	cancel context.CancelFunc,
	destination io.Writer,
	source io.ReadCloser,
	limit int64,
	policy artifactTransferPolicy,
) (int64, bool, error) {
	var lastProgress atomic.Int64
	lastProgress.Store(time.Now().UnixNano())
	var stalled atomic.Bool
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(policy.CheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				_ = source.Close()
				return
			case now := <-ticker.C:
				last := time.Unix(0, lastProgress.Load())
				if now.Sub(last) >= policy.StallTimeout {
					stalled.Store(true)
					cancel()
					_ = source.Close()
					return
				}
			}
		}
	}()
	meaningfulProgress := artifactMeaningfulProgress
	if relative := limit / 16; relative < meaningfulProgress {
		meaningfulProgress = relative
	}
	if meaningfulProgress < 1 {
		meaningfulProgress = 1
	}
	reader := &progressReader{
		reader:       source,
		lastProgress: &lastProgress,
		threshold:    meaningfulProgress,
	}
	written, err := io.CopyBuffer(destination, io.LimitReader(reader, limit), make([]byte, 256<<10))
	close(done)
	return written, stalled.Load(), err
}

func removeDisposableCandidate(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.IsDir() {
		return errors.New("candidate path is a directory")
	}
	return os.Remove(path)
}

func artifactExpectedSize(encoded string) (int64, error) {
	if encoded == "" {
		return 0, errors.New("artifact size is missing")
	}
	value, err := strconv.ParseInt(encoded, 10, 64)
	if err != nil || value <= 0 || value > maximumGatewayArtifactSize || strconv.FormatInt(value, 10) != encoded {
		return 0, errors.New("invalid artifact size")
	}
	return value, nil
}
