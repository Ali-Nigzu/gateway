package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const maximumReleaseBootstrapBytes = int64(1 << 20)

// TestReleaseBootstrapInput gives both native build scripts one exact parser
// for the private input they will embed. It validates the file without logging
// its contents.
func TestReleaseBootstrapInput(t *testing.T) {
	path := os.Getenv("CAMOS_VERIFY_BOOTSTRAP_CREDENTIAL")
	if path == "" {
		t.Skip("invoked by the native release build scripts")
	}
	if err := validateReleaseBootstrapInput(path); err != nil {
		t.Fatal(err)
	}
}

func validateReleaseBootstrapInput(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 ||
		info.Size() > maximumReleaseBootstrapBytes {
		return errors.New("release bootstrap credential is not a bounded regular file")
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return errors.New("release bootstrap credential could not be read")
	}
	if err := validateBootstrapCredentials(encoded); err != nil {
		return err
	}
	return nil
}

func TestReleaseBootstrapInputUsesRuntimeIdentityValidation(t *testing.T) {
	valid := `{"type":"service_account","project_id":"camosbase","client_email":"gateway-bootstrap@camosbase.iam.gserviceaccount.com","private_key":"private material","token_uri":"https://oauth2.googleapis.com/token"}`
	for _, test := range []struct {
		name    string
		encoded string
		valid   bool
	}{
		{"exact identity", valid, true},
		{"wrong account", strings.Replace(valid, "gateway-bootstrap@", "other@", 1), false},
		{"wrong project", strings.Replace(valid, `"project_id":"camosbase"`, `"project_id":"other"`, 1), false},
		{"wrong token host", strings.Replace(valid, "oauth2.googleapis.com", "example.com", 1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bootstrap_sa.json")
			if err := os.WriteFile(path, []byte(test.encoded), 0o600); err != nil {
				t.Fatal(err)
			}
			err := validateReleaseBootstrapInput(path)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v error=%v", test.valid, err)
			}
		})
	}
}

var releaseArtifactTargets = []string{
	"windows-amd64.exe",
	"darwin-arm64",
	"darwin-amd64",
	"linux-amd64",
}

// TestReleaseBuildArtifact is invoked by the native build scripts after go
// build. It statically verifies the artifact; it never executes candidate code.
func TestReleaseBuildArtifact(t *testing.T) {
	path := os.Getenv("CAMOS_VERIFY_RELEASE_ARTIFACT")
	target := os.Getenv("CAMOS_VERIFY_RELEASE_TARGET")
	version := os.Getenv("CAMOS_VERIFY_RELEASE_VERSION")
	if path == "" && target == "" && version == "" {
		t.Skip("invoked by the native release build scripts")
	}
	if path == "" || target == "" || version == "" {
		t.Fatal("CAMOS_VERIFY_RELEASE_ARTIFACT, CAMOS_VERIFY_RELEASE_TARGET and CAMOS_VERIFY_RELEASE_VERSION must be set together")
	}
	if version != BuildVersion {
		t.Fatalf("build script expects version %q but source BuildVersion is %q", version, BuildVersion)
	}
	identity, err := inspectGatewayCandidate(path, version, target)
	if err != nil {
		t.Fatalf("release artifact failed static identity verification: %v", err)
	}
	if identity.Version != BuildVersion || identity.Target != target || identity.Module != gatewayGoModule {
		t.Fatalf("release identity mismatch: %#v", identity)
	}
	t.Logf("verified %s: version=%s target=%s size=%d sha256=%x", filepath.Base(path), identity.Version, identity.Target, identity.Size, identity.SHA256)
}

// TestLocalReleaseSet is the pre-upload admission check for an assembled set
// of all four native artifacts and its aggregate SHA256SUMS manifest.
func TestLocalReleaseSet(t *testing.T) {
	if os.Getenv("CAMOS_LOCAL_RELEASE_VERIFY") != "1" {
		t.Skip("set CAMOS_LOCAL_RELEASE_VERIFY=1 for assembled release verification")
	}
	directory := os.Getenv("CAMOS_GATEWAY_RELEASE_DIR")
	if directory == "" {
		t.Fatal("CAMOS_GATEWAY_RELEASE_DIR is required")
	}
	verifyLocalReleaseSet(t, directory, BuildVersion)
}

func verifyLocalReleaseSet(t *testing.T, directory, version string) map[string]gatewayCandidateIdentity {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatalf("read release directory: %v", err)
	}
	allowed := map[string]bool{"SHA256SUMS": true}
	for _, target := range releaseArtifactTargets {
		allowed[target] = true
	}
	if len(entries) != len(allowed) {
		t.Fatalf("release directory must contain only SHA256SUMS and the four supported artifacts (found %d entries)", len(entries))
	}
	for _, entry := range entries {
		if !allowed[entry.Name()] || !entry.Type().IsRegular() {
			t.Fatalf("release directory contains unexpected entry %q", entry.Name())
		}
	}
	manifest, err := readReleaseChecksumManifest(filepath.Join(directory, "SHA256SUMS"))
	if err != nil {
		t.Fatal(err)
	}
	identities := make(map[string]gatewayCandidateIdentity, len(releaseArtifactTargets))
	for _, target := range releaseArtifactTargets {
		identity, err := inspectGatewayCandidate(filepath.Join(directory, target), version, target)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if identity.SHA256 != manifest[target] {
			t.Fatalf("%s does not match its SHA256SUMS entry", target)
		}
		identities[target] = identity
	}
	return identities
}

func readReleaseChecksumManifest(path string) (map[string][sha256.Size]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("release checksum manifest: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 4096 {
		return nil, errors.New("release checksum manifest is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("release checksum manifest: %w", err)
	}
	defer file.Close()

	allowed := make(map[string]bool, len(releaseArtifactTargets))
	for _, target := range releaseArtifactTargets {
		allowed[target] = true
	}
	checksums := make(map[string][sha256.Size]byte, len(releaseArtifactTargets))
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		separator := strings.Index(line, "  ")
		if separator != 64 || len(line) <= separator+2 || strings.Contains(line[separator+2:], " ") {
			return nil, errors.New("release checksum manifest contains a malformed entry")
		}
		hashText, target := line[:separator], line[separator+2:]
		if !allowed[target] {
			return nil, fmt.Errorf("release checksum manifest contains unknown target %q", target)
		}
		if _, exists := checksums[target]; exists {
			return nil, fmt.Errorf("release checksum manifest contains duplicate target %q", target)
		}
		if hashText != strings.ToLower(hashText) {
			return nil, errors.New("release checksum manifest SHA-256 is not canonical lowercase hexadecimal")
		}
		decoded, err := hex.DecodeString(hashText)
		if err != nil || len(decoded) != sha256.Size {
			return nil, errors.New("release checksum manifest contains invalid SHA-256")
		}
		var digest [sha256.Size]byte
		copy(digest[:], decoded)
		checksums[target] = digest
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read release checksum manifest: %w", err)
	}
	if len(checksums) != len(releaseArtifactTargets) {
		return nil, errors.New("release checksum manifest must contain exactly all four supported targets")
	}
	return checksums, nil
}

func TestReadReleaseChecksumManifestRequiresExactFourTargets(t *testing.T) {
	zero := strings.Repeat("0", sha256.Size*2)
	valid := ""
	for _, target := range releaseArtifactTargets {
		valid += zero + "  " + target + "\n"
	}
	tests := []struct {
		name    string
		content string
		valid   bool
	}{
		{name: "exact release set", content: valid, valid: true},
		{name: "missing target", content: strings.TrimSuffix(valid, zero+"  linux-amd64\n")},
		{name: "duplicate target", content: valid + zero + "  linux-amd64\n"},
		{name: "unknown target", content: valid + zero + "  linux-arm64\n"},
		{name: "noncanonical uppercase", content: strings.Replace(valid, zero, strings.Repeat("A", sha256.Size*2), 1)},
		{name: "malformed hash", content: strings.Replace(valid, zero, strings.Repeat("g", sha256.Size*2), 1)},
		{name: "single separator", content: strings.Replace(valid, "  windows-amd64.exe", " windows-amd64.exe", 1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "SHA256SUMS")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := readReleaseChecksumManifest(path)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
		})
	}
}
