package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeSourcesContainNoLegacyCredentialOrSchemaDependency(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		encoded, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		source := string(encoded)
		for _, forbidden := range []string{
			`"sa.json"`,
			"gcs_source_uri",
			"rtsp_config",
			"capture_config",
			"analysis_config",
			"analysis_interval_minutes",
			"bigquery_destination",
			"gateway_last_seen_at",
		} {
			if strings.Contains(source, forbidden) {
				t.Fatalf("%s contains legacy dependency %q", name, forbidden)
			}
		}
	}
}

func TestEveryNativeServiceClearsStaleFramePackagesBeforeRuntimeStartup(t *testing.T) {
	for _, name := range []string{
		"service_windows.go",
		"service_linux.go",
		"service_darwin.go",
	} {
		encoded, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		source := string(encoded)
		cleanup := strings.Index(source, "clearStaleFramePackageState()")
		ffmpeg := strings.Index(source, "installedFFmpegPath()")
		credentials := strings.Index(source, "newRuntimeCredentials(")
		if cleanup < 0 || ffmpeg < 0 || credentials < 0 ||
			cleanup > ffmpeg || cleanup > credentials {
			t.Fatalf("%s does not clear stale frame packages before runtime startup", name)
		}
	}
}

func TestEveryNativeServiceRecoversUpdatesBeforeOptionalRuntimeStartup(t *testing.T) {
	for _, name := range []string{
		"service_windows.go",
		"service_linux.go",
		"service_darwin.go",
	} {
		encoded, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		source := string(encoded)
		removal := strings.Index(source, "posixRemovalPending()")
		if name == "service_windows.go" {
			removal = strings.Index(source, "windowsRemovalAuthorized()")
		}
		recovery := strings.Index(source, "reconcileUpdateStateAtStartup(")
		ffmpeg := strings.Index(source, "installedFFmpegPath()")
		credentials := strings.Index(source, "newRuntimeCredentials(")
		if removal < 0 || recovery < 0 || ffmpeg < 0 || credentials < 0 ||
			removal > recovery || recovery > ffmpeg || recovery > credentials {
			t.Fatalf("%s does not recover updates after removal authority and before optional runtime startup", name)
		}
	}
}
