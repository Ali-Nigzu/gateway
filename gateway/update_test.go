package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var fastArtifactTransferPolicy = artifactTransferPolicy{
	StallTimeout:    150 * time.Millisecond,
	AbsoluteTimeout: 3 * time.Second,
	CheckInterval:   10 * time.Millisecond,
}

func TestArtifactPlatformFilename(t *testing.T) {
	tests := []struct{ goos, goarch, filename string }{
		{"windows", "amd64", "windows-amd64.exe"},
		{"darwin", "arm64", "darwin-arm64"},
		{"darwin", "amd64", "darwin-amd64"},
		{"linux", "amd64", "linux-amd64"},
	}
	for _, test := range tests {
		actual, err := artifactPlatformFilename(test.goos, test.goarch)
		if err != nil || actual != test.filename {
			t.Fatalf("%s/%s = %q, %v", test.goos, test.goarch, actual, err)
		}
	}
	for _, unsupported := range [][2]string{{"linux", "arm64"}, {"windows", "arm64"}, {"freebsd", "amd64"}, {"windows", "386"}} {
		if _, err := artifactPlatformFilename(unsupported[0], unsupported[1]); err == nil {
			t.Fatalf("unsupported tuple %v was accepted", unsupported)
		}
	}
}

func TestValidateArtifactVersionUsesUnambiguousProviderSubset(t *testing.T) {
	for _, version := range []string{"1.0", "release-1", "1+build.2", "a~b"} {
		if err := validateArtifactVersion(version); err != nil {
			t.Errorf("valid version %q rejected: %v", version, err)
		}
	}
	for _, version := range []string{"", ".1", "1.", "Release-1", "release_1", "1:2", "1/2", strings.Repeat("a", 129)} {
		if err := validateArtifactVersion(version); err == nil {
			t.Errorf("invalid or ambiguous provider version %q accepted", version)
		}
	}
}

func TestArtifactFileIDParsesRealGenericIdentity(t *testing.T) {
	tests := []struct {
		name string
		want genericArtifactFileID
		ok   bool
	}{
		{artifactRegistryRepository + "/files/gateway:1.0:windows-amd64.exe", genericArtifactFileID{"gateway", "1.0", "windows-amd64.exe"}, true},
		{artifactRegistryRepository + "/files/gateway%3A1.0%3Awindows-amd64.exe", genericArtifactFileID{"gateway", "1.0", "windows-amd64.exe"}, true},
		{"projects/other/locations/europe-west2/repositories/camos-gateway-prod/files/gateway:1.0:windows-amd64.exe", genericArtifactFileID{}, false},
		{artifactRegistryRepository + "/files/other:1.0:windows-amd64.exe", genericArtifactFileID{"other", "1.0", "windows-amd64.exe"}, true},
		{artifactRegistryRepository + "/files/gateway:2.0:windows-amd64.exe", genericArtifactFileID{"gateway", "2.0", "windows-amd64.exe"}, true},
		{artifactRegistryRepository + "/files/nested/gateway:1.0:windows-amd64.exe", genericArtifactFileID{}, false},
		{artifactRegistryRepository + "/files/gateway:1.0:nested%2Fwindows-amd64.exe", genericArtifactFileID{}, false},
		{artifactRegistryRepository + "/files/windows-amd64.exe", genericArtifactFileID{}, false},
		{artifactRegistryRepository + "/files/gateway:1.0:windows:amd64.exe", genericArtifactFileID{}, false},
	}
	for _, test := range tests {
		actual, ok := artifactFileID(test.name)
		if ok != test.ok || actual != test.want {
			t.Errorf("artifactFileID(%q) = %#v, %t; want %#v, %t", test.name, actual, ok, test.want, test.ok)
		}
	}
}

func TestSelectArtifactFileRequiresExactContract(t *testing.T) {
	digest := sha256.Sum256([]byte("candidate"))
	wanted := validArtifactFile("1.0", "windows-amd64.exe", int64(len("candidate")), digest)
	selected, hash, err := selectArtifactFile([]artifactFile{
		validArtifactFile("1.0", "linux-amd64", 10, digest),
		{Name: artifactRegistryRepository + "/files/other:1.0:windows-amd64.exe", Owner: artifactVersionOwner("1.0")},
		wanted,
	}, "1.0", "windows-amd64.exe")
	if err != nil || selected.Name != wanted.Name || !bytes.Equal(hash, digest[:]) {
		t.Fatalf("selection = %#v, %x, %v", selected, hash, err)
	}

	wrong := []artifactFile{
		{Name: strings.Replace(wanted.Name, artifactRegistryRepository, "projects/x/locations/x/repositories/x", 1), Owner: wanted.Owner, SizeBytes: wanted.SizeBytes, Hashes: wanted.Hashes},
		{Name: strings.Replace(wanted.Name, "gateway:1.0", "other:1.0", 1), Owner: wanted.Owner, SizeBytes: wanted.SizeBytes, Hashes: wanted.Hashes},
		{Name: strings.Replace(wanted.Name, ":1.0:", ":2.0:", 1), Owner: wanted.Owner, SizeBytes: wanted.SizeBytes, Hashes: wanted.Hashes},
		{Name: wanted.Name, Owner: artifactVersionOwner("2.0"), SizeBytes: wanted.SizeBytes, Hashes: wanted.Hashes},
		{Name: strings.Replace(wanted.Name, "windows-amd64.exe", "other.exe", 1), Owner: wanted.Owner, SizeBytes: wanted.SizeBytes, Hashes: wanted.Hashes},
	}
	for _, file := range wrong {
		if _, _, err := selectArtifactFile([]artifactFile{file}, "1.0", "windows-amd64.exe"); err == nil {
			t.Fatalf("wrong contract was accepted: %#v", file)
		}
	}
	if _, _, err := selectArtifactFile([]artifactFile{wanted, wanted}, "1.0", "windows-amd64.exe"); err == nil {
		t.Fatal("duplicate exact artifact was accepted")
	}
}

func TestSelectArtifactFileRequiresOneCanonicalSHAAndSize(t *testing.T) {
	digest := sha256.Sum256([]byte("candidate"))
	valid := validArtifactFile("1.0", "windows-amd64.exe", 9, digest)
	tests := []struct {
		name   string
		mutate func(*artifactFile)
		want   artifactFailureCategory
	}{
		{"missing size", func(file *artifactFile) { file.SizeBytes = "" }, artifactFailureSize},
		{"zero size", func(file *artifactFile) { file.SizeBytes = "0" }, artifactFailureSize},
		{"noncanonical size", func(file *artifactFile) { file.SizeBytes = "09" }, artifactFailureSize},
		{"missing sha", func(file *artifactFile) { file.Hashes = nil }, artifactFailureSHA},
		{"malformed sha", func(file *artifactFile) { file.Hashes[0].Value = "not-base64" }, artifactFailureSHA},
		{"short sha", func(file *artifactFile) { file.Hashes[0].Value = base64.StdEncoding.EncodeToString([]byte("short")) }, artifactFailureSHA},
		{"duplicate sha", func(file *artifactFile) { file.Hashes = append(file.Hashes, file.Hashes[0]) }, artifactFailureSHA},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			file := valid
			file.Hashes = append([]artifactHash(nil), valid.Hashes...)
			test.mutate(&file)
			_, _, err := selectArtifactFile([]artifactFile{file}, "1.0", "windows-amd64.exe")
			if err == nil || artifactFailureCategoryOf(err) != test.want {
				t.Fatalf("error = %v, category = %q", err, artifactFailureCategoryOf(err))
			}
		})
	}
	withMD5 := valid
	withMD5.Hashes = append(withMD5.Hashes, artifactHash{Type: "MD5", Value: "ignored"})
	if _, _, err := selectArtifactFile([]artifactFile{withMD5}, "1.0", "windows-amd64.exe"); err != nil {
		t.Fatalf("provider non-authority hash was rejected: %v", err)
	}
}

func TestCapturedRealProviderFixture(t *testing.T) {
	fixture, err := os.Open(filepath.Join("testdata", "artifact_registry_generic_files_real_sanitized.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer fixture.Close()
	page, err := decodeArtifactMetadata(fixture)
	if err != nil {
		t.Fatal(err)
	}
	selected, hash, err := selectArtifactFile(page.Files, "2", "windows-amd64.exe")
	if err != nil {
		t.Fatal(err)
	}
	identity, ok := artifactFileID(selected.Name)
	if !ok || identity != (genericArtifactFileID{"gateway", "2", "windows-amd64.exe"}) || len(hash) != sha256.Size {
		t.Fatalf("captured provider identity = %#v, %t, sha bytes %d", identity, ok, len(hash))
	}
}

func TestDownloadGatewayCandidatePublishesVerifiedBytes(t *testing.T) {
	payload := []byte("whole executable bytes")
	server := newArtifactTestServer(t, "1.0", "windows-amd64.exe", payload, nil)
	defer server.Close()
	withArtifactEndpoint(t, server.URL)
	candidate := filepath.Join(t.TempDir(), "candidate")
	if err := os.WriteFile(candidate, []byte("stale candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate+".downloading", []byte("stale partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := downloadTestCandidate(context.Background(), server.Client(), "1.0", candidate, "windows-amd64.exe", fastArtifactTransferPolicy); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(candidate)
	if err != nil || !strings.EqualFold(string(actual), string(payload)) {
		t.Fatalf("candidate = %q, %v", actual, err)
	}
}

func TestDownloadAcceptsAbsentContentLength(t *testing.T) {
	payload := []byte("chunked executable")
	server := newArtifactTestServer(t, "1.0", "windows-amd64.exe", payload, func(writer http.ResponseWriter, request *http.Request) bool {
		if strings.HasPrefix(request.URL.Path, "/download/") {
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			_, _ = writer.Write(payload)
			return true
		}
		return false
	})
	defer server.Close()
	withArtifactEndpoint(t, server.URL)
	if err := downloadTestCandidate(context.Background(), server.Client(), "1.0", filepath.Join(t.TempDir(), "candidate"), "windows-amd64.exe", fastArtifactTransferPolicy); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadRejectsContentLengthMismatch(t *testing.T) {
	payload := []byte("payload")
	server := newArtifactTestServer(t, "1.0", "windows-amd64.exe", payload, func(writer http.ResponseWriter, request *http.Request) bool {
		if strings.HasPrefix(request.URL.Path, "/download/") {
			writer.Header().Set("Content-Length", "8")
			_, _ = writer.Write(payload)
			return true
		}
		return false
	})
	defer server.Close()
	withArtifactEndpoint(t, server.URL)
	err := downloadTestCandidate(context.Background(), server.Client(), "1.0", filepath.Join(t.TempDir(), "candidate"), "windows-amd64.exe", fastArtifactTransferPolicy)
	if err == nil || artifactFailureCategoryOf(err) != artifactFailureSize {
		t.Fatalf("Content-Length mismatch error = %v", err)
	}
}

func TestCorruptTruncatedAndOversizedDownloadsNeverPublish(t *testing.T) {
	tests := []struct {
		name            string
		approvedPayload []byte
		downloadPayload []byte
	}{
		{"wrong sha", []byte("approved"), []byte("corrupt!")},
		{"truncated", []byte("complete approved executable"), []byte("partial")},
		{"oversized", []byte("approved"), []byte("approved-extra")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newArtifactTestServer(t, "1.0", "windows-amd64.exe", test.approvedPayload, func(writer http.ResponseWriter, request *http.Request) bool {
				if strings.HasPrefix(request.URL.Path, "/download/") {
					writer.WriteHeader(http.StatusOK)
					writer.(http.Flusher).Flush()
					_, _ = writer.Write(test.downloadPayload)
					return true
				}
				return false
			})
			defer server.Close()
			withArtifactEndpoint(t, server.URL)
			directory := t.TempDir()
			candidate := filepath.Join(directory, "candidate")
			if err := downloadTestCandidate(context.Background(), server.Client(), "1.0", candidate, "windows-amd64.exe", fastArtifactTransferPolicy); err == nil {
				t.Fatal("invalid candidate was accepted")
			}
			if _, err := os.Stat(candidate); !os.IsNotExist(err) {
				t.Fatalf("candidate was published: %v", err)
			}
			if _, err := os.Stat(candidate + ".downloading"); !os.IsNotExist(err) {
				t.Fatalf("partial file remains: %v", err)
			}
		})
	}
}

func TestSlowProgressContinuesButFullStallAborts(t *testing.T) {
	payload := []byte("slow-but-healthy")
	policy := artifactTransferPolicy{StallTimeout: 100 * time.Millisecond, AbsoluteTimeout: 2 * time.Second, CheckInterval: 10 * time.Millisecond}
	slow := newArtifactTestServer(t, "1.0", "windows-amd64.exe", payload, func(writer http.ResponseWriter, request *http.Request) bool {
		if !strings.HasPrefix(request.URL.Path, "/download/") {
			return false
		}
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		for _, value := range payload {
			_, _ = writer.Write([]byte{value})
			writer.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
		return true
	})
	defer slow.Close()
	withArtifactEndpoint(t, slow.URL)
	if err := downloadTestCandidate(context.Background(), slow.Client(), "1.0", filepath.Join(t.TempDir(), "candidate"), "windows-amd64.exe", policy); err != nil {
		t.Fatalf("healthy slow transfer failed: %v", err)
	}

	stall := newArtifactTestServer(t, "1.0", "windows-amd64.exe", payload, func(writer http.ResponseWriter, request *http.Request) bool {
		if !strings.HasPrefix(request.URL.Path, "/download/") {
			return false
		}
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
		return true
	})
	defer stall.Close()
	withArtifactEndpoint(t, stall.URL)
	err := downloadTestCandidate(context.Background(), stall.Client(), "1.0", filepath.Join(t.TempDir(), "candidate"), "windows-amd64.exe", policy)
	if err == nil || artifactFailureCategoryOf(err) != artifactFailureStall {
		t.Fatalf("stall error = %v, category %q", err, artifactFailureCategoryOf(err))
	}
}

func TestDownloadCancellationLeavesNoPartial(t *testing.T) {
	payload := []byte("cancel-me")
	server := newArtifactTestServer(t, "1.0", "windows-amd64.exe", payload, func(writer http.ResponseWriter, request *http.Request) bool {
		if !strings.HasPrefix(request.URL.Path, "/download/") {
			return false
		}
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		<-request.Context().Done()
		return true
	})
	defer server.Close()
	withArtifactEndpoint(t, server.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	candidate := filepath.Join(t.TempDir(), "candidate")
	if err := downloadTestCandidate(ctx, server.Client(), "1.0", candidate, "windows-amd64.exe", fastArtifactTransferPolicy); err == nil {
		t.Fatal("cancelled transfer succeeded")
	}
	if _, err := os.Stat(candidate + ".downloading"); !os.IsNotExist(err) {
		t.Fatalf("cancelled partial remains: %v", err)
	}
}

func TestNetworkDisconnectLeavesNoPartial(t *testing.T) {
	payload := []byte("complete-payload")
	server := newArtifactTestServer(t, "1.0", "windows-amd64.exe", payload, func(writer http.ResponseWriter, request *http.Request) bool {
		if !strings.HasPrefix(request.URL.Path, "/download/") {
			return false
		}
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return true
		}
		connection, buffered, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return true
		}
		_, _ = fmt.Fprintf(buffered, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\npartial", len(payload))
		_ = buffered.Flush()
		_ = connection.Close()
		return true
	})
	defer server.Close()
	withArtifactEndpoint(t, server.URL)
	candidate := filepath.Join(t.TempDir(), "candidate")
	if err := downloadTestCandidate(context.Background(), server.Client(), "1.0", candidate, "windows-amd64.exe", fastArtifactTransferPolicy); err == nil {
		t.Fatal("disconnected transfer succeeded")
	}
	if _, err := os.Stat(candidate + ".downloading"); !os.IsNotExist(err) {
		t.Fatalf("disconnected partial remains: %v", err)
	}
}

func TestAbsoluteTransferDurationIsBounded(t *testing.T) {
	payload := []byte("progress-that-never-finishes")
	server := newArtifactTestServer(t, "1.0", "windows-amd64.exe", payload, func(writer http.ResponseWriter, request *http.Request) bool {
		if !strings.HasPrefix(request.URL.Path, "/download/") {
			return false
		}
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		for {
			select {
			case <-request.Context().Done():
				return true
			case <-time.After(25 * time.Millisecond):
				_, _ = writer.Write([]byte{'x'})
				writer.(http.Flusher).Flush()
			}
		}
	})
	defer server.Close()
	withArtifactEndpoint(t, server.URL)
	policy := artifactTransferPolicy{StallTimeout: 90 * time.Millisecond, AbsoluteTimeout: 140 * time.Millisecond, CheckInterval: 10 * time.Millisecond}
	if err := downloadTestCandidate(context.Background(), server.Client(), "1.0", filepath.Join(t.TempDir(), "candidate"), "windows-amd64.exe", policy); err == nil {
		t.Fatal("transfer exceeded its absolute duration without failing")
	}
}

func TestArtifactMetadataIsStrictAndBounded(t *testing.T) {
	for _, encoded := range []string{
		`{"files":[]} {"files":[]}`,
		`{"files":[]`,
		`null`,
	} {
		if _, err := decodeArtifactMetadata(strings.NewReader(encoded)); err == nil {
			t.Fatalf("malformed metadata accepted: %q", encoded)
		}
	}
	tooLarge := io.LimitReader(strings.NewReader(strings.Repeat(" ", int(maximumArtifactMetadataBytes)+1)), maximumArtifactMetadataBytes+1)
	if _, err := decodeArtifactMetadata(tooLarge); err == nil {
		t.Fatal("oversized metadata accepted")
	}
}

func TestArtifactMetadataRejectsRepeatedPaginationToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"nextPageToken":"same"}`)
	}))
	defer server.Close()
	withArtifactEndpoint(t, server.URL)
	if _, err := listArtifactVersion(context.Background(), server.Client(), "1.0"); err == nil {
		t.Fatal("repeated metadata page token was accepted")
	}
}

func TestArtifactExpectedSizeIsMandatoryCanonicalAndBounded(t *testing.T) {
	if size, err := artifactExpectedSize("123"); err != nil || size != 123 {
		t.Fatalf("valid size = %d, error %v", size, err)
	}
	for _, invalid := range []string{"", "0", "01", "-1", "invalid", fmt.Sprint(maximumGatewayArtifactSize + 1)} {
		if _, err := artifactExpectedSize(invalid); err == nil {
			t.Fatalf("invalid artifact size %q was accepted", invalid)
		}
	}
}

func validArtifactFile(version, filename string, size int64, digest [sha256.Size]byte) artifactFile {
	return artifactFile{
		Name:      artifactRegistryRepository + "/files/" + artifactPackage + ":" + version + ":" + filename,
		SizeBytes: strconv.FormatInt(size, 10),
		Hashes:    []artifactHash{{Type: "SHA256", Value: base64.StdEncoding.EncodeToString(digest[:])}},
		Owner:     artifactVersionOwner(version),
	}
}

func newArtifactTestServer(
	t *testing.T,
	version, filename string,
	approvedPayload []byte,
	override func(http.ResponseWriter, *http.Request) bool,
) *httptest.Server {
	t.Helper()
	digest := sha256.Sum256(approvedPayload)
	owner := artifactVersionOwner(version)
	file := validArtifactFile(version, filename, int64(len(approvedPayload)), digest)
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if override != nil && override(writer, request) {
			return
		}
		switch request.URL.Path {
		case "/v1/" + artifactRegistryRepository + "/files":
			if request.URL.Query().Get("filter") != fmt.Sprintf("owner=\"%s\"", owner) || request.URL.Query().Get("pageSize") != "1000" {
				t.Errorf("unexpected metadata query: %s", request.URL.RawQuery)
			}
			_ = json.NewEncoder(writer).Encode(artifactFileList{Files: []artifactFile{file}})
		case "/download/v1/" + file.Name + ":download":
			if request.URL.Query().Get("alt") != "media" {
				t.Errorf("download alt = %q", request.URL.Query().Get("alt"))
			}
			_, _ = writer.Write(approvedPayload)
		default:
			http.NotFound(writer, request)
		}
	}))
}

// Kept behind helpers so the transfer tests exercise production publication
// and verification while candidate format tests exercise the real inspector.
func downloadTestCandidate(ctx context.Context, client *http.Client, version, path, target string, policy artifactTransferPolicy) error {
	return downloadGatewayCandidateForTarget(ctx, client, version, path, target, policy, func(path, version, target string) (gatewayCandidateIdentity, error) {
		payload, err := os.ReadFile(path)
		if err != nil {
			return gatewayCandidateIdentity{}, err
		}
		identity := gatewayCandidateIdentity{Version: version, Target: target, Size: int64(len(payload)), SHA256: sha256.Sum256(payload)}
		return identity, nil
	})
}

func withArtifactEndpoint(t *testing.T, endpoint string) {
	t.Helper()
	original := artifactRegistryEndpoint
	artifactRegistryEndpoint = endpoint
	t.Cleanup(func() { artifactRegistryEndpoint = original })
}
