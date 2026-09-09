package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

var (
	testRemovalGatewayID      = uuid.MustParse("1d91378f-7b96-4e6f-95b2-1304b728d28f")
	testOtherRemovalGatewayID = uuid.MustParse("ea4ee969-8484-48c4-9bcd-8de95815d9ab")
)

func TestRemovalMarkerV1IsExactAndGatewayBound(t *testing.T) {
	encoded, err := createRemovalMarker(testRemovalGatewayID)
	if err != nil {
		t.Fatal(err)
	}
	want := removalMarkerV1Prefix + testRemovalGatewayID.String() + "\n"
	if string(encoded) != want {
		t.Fatalf("marker = %q, want %q", encoded, want)
	}
	marker, err := parseRemovalMarker(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if marker.legacy || marker.gatewayID != testRemovalGatewayID {
		t.Fatalf("parsed marker = %#v", marker)
	}
	if err := validateRemovalMarker(marker, testRemovalGatewayID); err != nil {
		t.Fatal(err)
	}
	if err := validateRemovalMarker(marker, testOtherRemovalGatewayID); err == nil {
		t.Fatal("marker was accepted for a different GatewayID")
	}
	if err := validateRemovalMarker(marker, uuid.Nil); err == nil {
		t.Fatal("marker was accepted without a current GatewayID")
	}
	if _, err := createRemovalMarker(uuid.Nil); err == nil {
		t.Fatal("nil GatewayID was encoded")
	}
}

func TestRemovalMarkerAcceptsOnlyExactLegacyValue(t *testing.T) {
	marker, err := parseRemovalMarker([]byte(legacyRemovalMarkerContents))
	if err != nil {
		t.Fatal(err)
	}
	if !marker.legacy || marker.gatewayID != uuid.Nil {
		t.Fatalf("legacy marker = %#v", marker)
	}
	if err := validateRemovalMarker(marker, testRemovalGatewayID); err != nil {
		t.Fatalf("legacy continuation was rejected: %v", err)
	}
}

func TestRemovalMarkerRejectsMalformedValues(t *testing.T) {
	valid := removalMarkerV1Prefix + testRemovalGatewayID.String() + "\n"
	for name, invalid := range map[string][]byte{
		"empty":                  nil,
		"legacy missing newline": []byte("terminal-removal"),
		"legacy suffix":          []byte(legacyRemovalMarkerContents + "extra"),
		"wrong purpose":          []byte("update\n"),
		"v1 missing newline":     []byte(valid[:len(valid)-1]),
		"v1 crlf":                []byte(valid[:len(valid)-1] + "\r\n"),
		"v1 suffix":              []byte(valid + "extra"),
		"v1 uppercase UUID":      []byte(removalMarkerV1Prefix + "1D91378F-7B96-4E6F-95B2-1304B728D28F\n"),
		"v1 nil UUID":            []byte(removalMarkerV1Prefix + uuid.Nil.String() + "\n"),
		"v1 braced UUID":         []byte(removalMarkerV1Prefix + "{1d91378f-7b96-4e6f-95b2-1304b728d28f}\n"),
		"unknown version":        []byte("terminal-removal:v2:" + testRemovalGatewayID.String() + "\n"),
		"leading data":           []byte("x" + valid),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseRemovalMarker(invalid); err == nil {
				t.Fatalf("invalid marker %q was accepted", invalid)
			}
		})
	}
}

func TestPOSIXRemovalStartupFailsClosedOnOrphanedNativeState(t *testing.T) {
	for _, test := range []struct {
		name          string
		markerPending bool
		nativePending bool
		wantPending   bool
		wantError     bool
	}{
		{"normal startup", false, false, false, false},
		{"committed marker", true, false, true, false},
		{"committed marker and native state", true, true, true, false},
		{"orphaned native state", false, true, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pending, err := decidePOSIXRemovalStartup(test.markerPending, test.nativePending)
			if pending != test.wantPending || (err != nil) != test.wantError {
				t.Fatalf("pending=%v err=%v, want pending=%v error=%v", pending, err, test.wantPending, test.wantError)
			}
		})
	}
}

func TestReadAndInspectGatewayRemovalMarkerFailClosed(t *testing.T) {
	directory := t.TempDir()
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))

	if marker, present, err := readRemovalMarker(paths.removalPending); err != nil || present {
		t.Fatalf("absent marker = %#v, %v, %v", marker, present, err)
	}
	if present, err := inspectGatewayRemovalMarker(paths, testRemovalGatewayID); err != nil || present {
		t.Fatalf("absent inspection = %v, %v", present, err)
	}

	encoded, err := createRemovalMarker(testRemovalGatewayID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.removalPending, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if present, err := inspectGatewayRemovalMarker(paths, testRemovalGatewayID); err != nil || !present {
		t.Fatalf("matching inspection = %v, %v", present, err)
	}
	if present, err := inspectGatewayRemovalMarker(paths, testOtherRemovalGatewayID); err == nil || present {
		t.Fatalf("mismatched inspection = %v, %v", present, err)
	}
	if present, err := inspectGatewayRemovalMarker(paths, uuid.Nil); err == nil || present {
		t.Fatalf("nil-ID inspection = %v, %v", present, err)
	}

	if err := os.WriteFile(paths.removalPending, []byte(legacyRemovalMarkerContents), 0o600); err != nil {
		t.Fatal(err)
	}
	if present, err := inspectGatewayRemovalMarker(paths, testRemovalGatewayID); err != nil || !present {
		t.Fatalf("legacy inspection = %v, %v", present, err)
	}

	if err := os.WriteFile(paths.removalPending, []byte("malformed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if present, err := inspectGatewayRemovalMarker(paths, testRemovalGatewayID); err == nil || present {
		t.Fatalf("malformed inspection = %v, %v", present, err)
	}

	if err := os.Remove(paths.removalPending); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(paths.removalPending, 0o700); err != nil {
		t.Fatal(err)
	}
	if marker, present, err := readRemovalMarker(paths.removalPending); err == nil || present {
		t.Fatalf("unreadable marker = %#v, %v, %v", marker, present, err)
	}
}

func TestCommitGatewayRemovalMarkerRefusesUnsafeExistingState(t *testing.T) {
	for name, existing := range map[string][]byte{
		"mismatched": mustRemovalMarker(t, testOtherRemovalGatewayID),
		"malformed":  []byte("malformed\n"),
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
			if err := os.WriteFile(paths.removalPending, existing, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := commitGatewayRemovalMarker(paths, testRemovalGatewayID); err == nil {
				t.Fatal("unsafe existing marker was overwritten")
			}
			actual, err := os.ReadFile(paths.removalPending)
			if err != nil {
				t.Fatal(err)
			}
			if string(actual) != string(existing) {
				t.Fatalf("existing marker changed from %q to %q", existing, actual)
			}
		})
	}

	for name, existing := range map[string][]byte{
		"matching": mustRemovalMarker(t, testRemovalGatewayID),
		"legacy":   []byte(legacyRemovalMarkerContents),
	} {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
			if err := os.WriteFile(paths.removalPending, existing, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := commitGatewayRemovalMarker(paths, testRemovalGatewayID); err != nil {
				t.Fatal(err)
			}
			actual, err := os.ReadFile(paths.removalPending)
			if err != nil {
				t.Fatal(err)
			}
			if string(actual) != string(existing) {
				t.Fatalf("idempotent commit changed %q to %q", existing, actual)
			}
		})
	}
}

func mustRemovalMarker(t *testing.T, gatewayID uuid.UUID) []byte {
	t.Helper()
	marker, err := createRemovalMarker(gatewayID)
	if err != nil {
		t.Fatal(err)
	}
	return marker
}

func TestPrepareIdentityForFinalRemovalKeepsOnlyMarkerAndHelper(t *testing.T) {
	directory := t.TempDir()
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	if err := os.Mkdir(paths.workDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(paths.workDirectory, "removal-helper")
	packageFrame := filepath.Join(
		paths.workDirectory,
		"frame-packages",
		"1",
		"1",
		"83",
		"window",
		"frame.jpg",
	)
	if err := os.MkdirAll(filepath.Dir(packageFrame), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{
		paths.gatewayID:   "gateway",
		paths.privateKey:  "key",
		paths.certificate: "certificate",
		helper:            "helper",
		packageFrame:      "jpeg",
		filepath.Join(paths.workDirectory, "stale-candidate"): "stale",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(paths.gatewayID, []byte(testRemovalGatewayID.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.removalPending, mustRemovalMarker(t, testRemovalGatewayID), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareIdentityForFinalRemoval(paths, helper); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || entries[0].Name() != "GatewayID" ||
		entries[1].Name() != removalPendingFilename || entries[2].Name() != identityWorkDirectory {
		t.Fatalf("identity entries after preparation = %v", entries)
	}
	workEntries, err := os.ReadDir(paths.workDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(workEntries) != 1 || workEntries[0].Name() != filepath.Base(helper) {
		t.Fatalf("work entries after preparation = %v", workEntries)
	}
	if _, err := os.Stat(packageFrame); !os.IsNotExist(err) {
		t.Fatalf("frame-package cache survived terminal preparation: %v", err)
	}
}

func TestPrepareIdentityForFinalRemovalAcceptsLegacyContinuation(t *testing.T) {
	directory := t.TempDir()
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	if err := os.Mkdir(paths.workDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(paths.workDirectory, "removal-helper")
	for path, contents := range map[string]string{
		paths.privateKey:     "key",
		paths.removalPending: legacyRemovalMarkerContents,
		helper:               "helper",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareIdentityForFinalRemoval(paths, helper); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.privateKey); !os.IsNotExist(err) {
		t.Fatalf("credential survived legacy terminal preparation: %v", err)
	}
}

func TestPrepareIdentityForFinalRemovalAcceptsCommittedV1AfterGatewayIDRemoval(t *testing.T) {
	directory := t.TempDir()
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	if err := os.Mkdir(paths.workDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(paths.workDirectory, "removal-helper")
	for path, contents := range map[string][]byte{
		paths.removalPending: mustRemovalMarker(t, testRemovalGatewayID),
		paths.privateKey:     []byte("partial-removal-residue"),
		helper:               []byte("helper"),
	} {
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareIdentityForFinalRemoval(paths, helper); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(paths.privateKey); !os.IsNotExist(err) {
		t.Fatalf("credential residue survived committed removal continuation: %v", err)
	}
}

func TestPrepareIdentityForFinalRemovalRejectsUnsafeMarkerState(t *testing.T) {
	for _, test := range []struct {
		name            string
		marker          []byte
		gatewayID       string
		markerDirectory bool
	}{
		{name: "missing marker", gatewayID: testRemovalGatewayID.String()},
		{name: "malformed marker", marker: []byte("malformed\n"), gatewayID: testRemovalGatewayID.String()},
		{name: "unreadable marker", gatewayID: testRemovalGatewayID.String(), markerDirectory: true},
		{name: "mismatched marker", marker: mustRemovalMarker(t, testOtherRemovalGatewayID), gatewayID: testRemovalGatewayID.String()},
		{name: "malformed GatewayID", marker: mustRemovalMarker(t, testRemovalGatewayID), gatewayID: "NOT-CANONICAL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
			if err := os.Mkdir(paths.workDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			helper := filepath.Join(paths.workDirectory, "removal-helper")
			credential := paths.privateKey
			for path, contents := range map[string]string{
				helper:     "helper",
				credential: "must-survive",
			} {
				if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.gatewayID != "" {
				if err := os.WriteFile(paths.gatewayID, []byte(test.gatewayID), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if test.markerDirectory {
				if err := os.Mkdir(paths.removalPending, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if test.marker != nil {
				if err := os.WriteFile(paths.removalPending, test.marker, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := prepareIdentityForFinalRemoval(paths, helper); err == nil {
				t.Fatal("unsafe removal state was accepted")
			}
			actual, err := os.ReadFile(credential)
			if err != nil || string(actual) != "must-survive" {
				t.Fatalf("credential was touched: %q, %v", actual, err)
			}
		})
	}
}

func TestPrepareIdentityForFinalRemovalRejectsHelperOutsideWorkDirectory(t *testing.T) {
	directory := t.TempDir()
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	if err := os.Mkdir(paths.workDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.removalPending, []byte(legacyRemovalMarkerContents), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(directory, "not-the-helper")
	if err := os.WriteFile(outside, []byte("unrelated"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := prepareIdentityForFinalRemoval(paths, outside); err == nil {
		t.Fatal("helper outside managed work directory was accepted")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file was touched: %v", err)
	}
}
