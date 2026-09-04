package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

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

func TestSelectArtifactFileRequiresExactOwnedFilenameAndSHA256(t *testing.T) {
	owner := artifactVersionOwner(2)
	digest := sha256.Sum256([]byte("candidate"))
	wanted := artifactFile{
		Name:   artifactRegistryRepository + "/files/windows-amd64.exe",
		Owner:  owner,
		Hashes: []artifactHash{{Type: "SHA256", Value: base64.StdEncoding.EncodeToString(digest[:])}},
	}
	files := []artifactFile{
		{Name: artifactRegistryRepository + "/files/other.exe", Owner: owner},
		{Name: wanted.Name, Owner: artifactVersionOwner(1)},
		wanted,
	}
	selected, hash, err := selectArtifactFile(files, owner, "windows-amd64.exe")
	if err != nil || selected.Name != wanted.Name || string(hash) != string(digest[:]) {
		t.Fatalf("selection = %#v, %x, %v", selected, hash, err)
	}
	if _, _, err := selectArtifactFile(append(files, wanted), owner, "windows-amd64.exe"); err == nil {
		t.Fatal("duplicate exact artifact was accepted")
	}
}

func TestDownloadGatewayCandidateVerifiesMetadataHash(t *testing.T) {
	payload := []byte("whole executable bytes")
	digest := sha256.Sum256(payload)
	filename, err := artifactPlatformFilename(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	owner := artifactVersionOwner(2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/v1/"+artifactRegistryRepository+"/files":
			fmt.Fprintf(writer, `{"files":[{"name":%q,"sizeBytes":%q,"owner":%q,"hashes":[{"type":"SHA256","value":%q}]}]}`,
				artifactRegistryRepository+"/files/"+filename,
				fmt.Sprint(len(payload)), owner, base64.StdEncoding.EncodeToString(digest[:]))
		case request.URL.Path == "/download/v1/"+artifactRegistryRepository+"/files/"+filename+":download":
			_, _ = writer.Write(payload)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	original := artifactRegistryEndpoint
	artifactRegistryEndpoint = server.URL
	t.Cleanup(func() { artifactRegistryEndpoint = original })

	candidate := filepath.Join(t.TempDir(), "candidate")
	if err := downloadGatewayCandidate(context.Background(), server.Client(), 2, candidate); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(candidate)
	if err != nil || string(actual) != string(payload) {
		t.Fatalf("candidate = %q, %v", actual, err)
	}
}

func TestCorruptDownloadLeavesActiveExecutableUntouched(t *testing.T) {
	payload := []byte("corrupt")
	expected := sha256.Sum256([]byte("approved"))
	filename, err := artifactPlatformFilename(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/"+artifactRegistryRepository+"/files" {
			fmt.Fprintf(writer, `{"files":[{"name":%q,"sizeBytes":%q,"owner":%q,"hashes":[{"type":"SHA256","value":%q}]}]}`,
				artifactRegistryRepository+"/files/"+filename,
				fmt.Sprint(len(payload)), artifactVersionOwner(2), base64.StdEncoding.EncodeToString(expected[:]))
			return
		}
		_, _ = writer.Write(payload)
	}))
	defer server.Close()
	original := artifactRegistryEndpoint
	artifactRegistryEndpoint = server.URL
	t.Cleanup(func() { artifactRegistryEndpoint = original })

	directory := t.TempDir()
	active := filepath.Join(directory, "active")
	candidate := filepath.Join(directory, "candidate")
	if err := os.WriteFile(active, []byte("working release"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := downloadGatewayCandidate(context.Background(), server.Client(), 2, candidate); err == nil {
		t.Fatal("corrupt candidate was accepted")
	}
	actual, err := os.ReadFile(active)
	if err != nil || string(actual) != "working release" {
		t.Fatalf("active executable changed: %q, %v", actual, err)
	}
	if _, err := os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatalf("candidate remains after failure: %v", err)
	}
}

func TestPartialDownloadNeverPublishesCandidate(t *testing.T) {
	approved := []byte("complete approved executable")
	partial := approved[:8]
	digest := sha256.Sum256(approved)
	filename, err := artifactPlatformFilename(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/"+artifactRegistryRepository+"/files" {
			fmt.Fprintf(writer, `{"files":[{"name":%q,"sizeBytes":%q,"owner":%q,"hashes":[{"type":"SHA256","value":%q}]}]}`,
				artifactRegistryRepository+"/files/"+filename,
				fmt.Sprint(len(approved)), artifactVersionOwner(2), base64.StdEncoding.EncodeToString(digest[:]))
			return
		}
		_, _ = writer.Write(partial)
	}))
	defer server.Close()
	original := artifactRegistryEndpoint
	artifactRegistryEndpoint = server.URL
	t.Cleanup(func() { artifactRegistryEndpoint = original })

	candidate := filepath.Join(t.TempDir(), "candidate")
	if err := downloadGatewayCandidate(context.Background(), server.Client(), 2, candidate); err == nil {
		t.Fatal("partial download was accepted")
	}
	if _, err := os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatalf("partial candidate was published: %v", err)
	}
	if _, err := os.Stat(candidate + ".downloading"); !os.IsNotExist(err) {
		t.Fatalf("partial temporary file remains: %v", err)
	}
}

func TestArtifactMetadataRejectsRepeatedPaginationToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(writer, `{"nextPageToken":"same"}`)
	}))
	defer server.Close()
	original := artifactRegistryEndpoint
	artifactRegistryEndpoint = server.URL
	t.Cleanup(func() { artifactRegistryEndpoint = original })
	if _, err := listArtifactVersion(context.Background(), server.Client(), 2); err == nil {
		t.Fatal("repeated metadata page token was accepted")
	}
}
