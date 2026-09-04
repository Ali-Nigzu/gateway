package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateEmbeddedReleaseRequiresBothAssets(t *testing.T) {
	originalBootstrap, originalFFmpeg := embeddedBootstrapCredential, embeddedFFmpeg
	t.Cleanup(func() {
		embeddedBootstrapCredential, embeddedFFmpeg = originalBootstrap, originalFFmpeg
	})
	embeddedBootstrapCredential, embeddedFFmpeg = nil, nil
	if err := validateEmbeddedRelease(); err == nil {
		t.Fatal("missing assets were accepted")
	}
	embeddedBootstrapCredential = []byte("bootstrap")
	if err := validateEmbeddedRelease(); err == nil {
		t.Fatal("missing FFmpeg was accepted")
	}
	embeddedFFmpeg = []byte("ffmpeg")
	if err := validateEmbeddedRelease(); err != nil {
		t.Fatalf("complete assets rejected: %v", err)
	}
}

func TestEnsureEmbeddedFFmpegReplacesDifferentPayload(t *testing.T) {
	original := embeddedFFmpeg
	t.Cleanup(func() { embeddedFFmpeg = original })
	embeddedFFmpeg = []byte("release payload")
	path := filepath.Join(t.TempDir(), ffmpegExecutableName)
	if err := os.WriteFile(path, []byte("old payload"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureEmbeddedFFmpeg(path); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "release payload" {
		t.Fatalf("installed payload = %q", contents)
	}
	if _, err := os.Stat(path + ".installing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary file remains: %v", err)
	}
}
