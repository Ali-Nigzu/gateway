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
			"bigquery_destination",
			"gateway_last_seen_at",
		} {
			if strings.Contains(source, forbidden) {
				t.Fatalf("%s contains legacy dependency %q", name, forbidden)
			}
		}
	}
}
