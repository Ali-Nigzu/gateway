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
)

const (
	artifactRegistryBase       = "https://artifactregistry.googleapis.com"
	artifactRegistryRepository = "projects/camosbase/locations/europe-west2/repositories/camos-gateway-prod"
	artifactPackage            = "gateway"
	maximumGatewayArtifactSize = int64(2 << 30)
)

var artifactRegistryEndpoint = artifactRegistryBase

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

func artifactVersionOwner(version int16) string {
	return fmt.Sprintf("%s/packages/%s/versions/%d", artifactRegistryRepository, artifactPackage, version)
}

func artifactFileID(name string) (string, bool) {
	prefix := artifactRegistryRepository + "/files/"
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	value, err := url.PathUnescape(strings.TrimPrefix(name, prefix))
	return value, err == nil
}

func selectArtifactFile(files []artifactFile, owner, filename string) (artifactFile, []byte, error) {
	var selected *artifactFile
	for index := range files {
		file := &files[index]
		id, valid := artifactFileID(file.Name)
		if file.Owner != owner || !valid || id != filename {
			continue
		}
		if selected != nil {
			return artifactFile{}, nil, errors.New("Artifact Registry returned duplicate exact files")
		}
		selected = file
	}
	if selected == nil {
		return artifactFile{}, nil, errors.New("requested Gateway artifact is missing")
	}
	for _, hash := range selected.Hashes {
		if hash.Type != "SHA256" {
			continue
		}
		value, err := base64.StdEncoding.DecodeString(hash.Value)
		if err != nil || len(value) != sha256.Size {
			return artifactFile{}, nil, errors.New("Gateway artifact SHA-256 is invalid")
		}
		return *selected, value, nil
	}
	return artifactFile{}, nil, errors.New("Gateway artifact SHA-256 is missing")
}

func listArtifactVersion(
	ctx context.Context,
	client *http.Client,
	version int16,
) ([]artifactFile, error) {
	owner := artifactVersionOwner(version)
	endpoint := artifactRegistryEndpoint + "/v1/" + artifactRegistryRepository + "/files"
	var files []artifactFile
	pageToken := ""
	seenPageTokens := make(map[string]struct{})
	for {
		values := url.Values{
			"filter":   {fmt.Sprintf("owner=\"%s\"", owner)},
			"pageSize": {"1000"},
		}
		if pageToken != "" {
			values.Set("pageToken", pageToken)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+values.Encode(), nil)
		if err != nil {
			return nil, err
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, errors.New("Artifact Registry metadata request failed")
		}
		var page artifactFileList
		decodeErr := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&page)
		closeErr := response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("Artifact Registry metadata request failed with status %d", response.StatusCode)
		}
		if decodeErr != nil || closeErr != nil {
			return nil, errors.New("Artifact Registry metadata response is invalid")
		}
		files = append(files, page.Files...)
		if page.NextPageToken == "" {
			return files, nil
		}
		if _, repeated := seenPageTokens[page.NextPageToken]; repeated {
			return nil, errors.New("Artifact Registry metadata pagination is invalid")
		}
		seenPageTokens[page.NextPageToken] = struct{}{}
		pageToken = page.NextPageToken
	}
}

func downloadGatewayCandidate(
	ctx context.Context,
	client *http.Client,
	desiredVersion int16,
	candidatePath string,
) error {
	if desiredVersion <= 0 {
		return errors.New("desired Gateway version is invalid")
	}
	filename, err := artifactPlatformFilename(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	files, err := listArtifactVersion(ctx, client, desiredVersion)
	if err != nil {
		return err
	}
	file, expectedHash, err := selectArtifactFile(files, artifactVersionOwner(desiredVersion), filename)
	if err != nil {
		return err
	}
	expectedSize, sizeKnown, err := artifactExpectedSize(file.SizeBytes)
	if err != nil {
		return errors.New("Gateway artifact size is invalid")
	}

	temporaryPath := candidatePath + ".downloading"
	_ = os.Remove(temporaryPath)
	_ = os.Remove(candidatePath)
	temporary, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return fmt.Errorf("Gateway candidate creation failed: %w", err)
	}
	keep := true
	defer func() {
		_ = temporary.Close()
		if keep {
			_ = os.Remove(temporaryPath)
		}
	}()

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		artifactRegistryEndpoint+"/download/v1/"+file.Name+":download?alt=media",
		nil,
	)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("Gateway artifact download failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Gateway artifact download failed with status %d", response.StatusCode)
	}
	hash := sha256.New()
	downloadLimit := maximumGatewayArtifactSize + 1
	if sizeKnown {
		downloadLimit = expectedSize + 1
	}
	written, err := io.Copy(
		io.MultiWriter(temporary, hash),
		io.LimitReader(response.Body, downloadLimit),
	)
	if err != nil {
		return errors.New("Gateway artifact download was incomplete")
	}
	if written <= 0 || written > maximumGatewayArtifactSize ||
		(sizeKnown && written != expectedSize) {
		return errors.New("Gateway artifact size does not match metadata")
	}
	if !bytes.Equal(hash.Sum(nil), expectedHash) {
		return errors.New("Gateway artifact SHA-256 mismatch")
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("Gateway candidate sync failed: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("Gateway candidate close failed: %w", err)
	}
	if err := secureInstalledExecutable(temporaryPath); err != nil {
		return fmt.Errorf("Gateway candidate permissions failed: %w", err)
	}
	if err := replaceFileAtomically(temporaryPath, candidatePath); err != nil {
		return fmt.Errorf("Gateway candidate publication failed: %w", err)
	}
	keep = false
	return syncParentDirectory(candidatePath)
}

func artifactExpectedSize(encoded string) (int64, bool, error) {
	if encoded == "" {
		return 0, false, nil
	}
	value, err := strconv.ParseInt(encoded, 10, 64)
	if err != nil || value <= 0 || value > maximumGatewayArtifactSize {
		return 0, false, errors.New("invalid artifact size")
	}
	return value, true, nil
}
