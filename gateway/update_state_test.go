package main

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func testPendingUpdateRecord() updatePendingRecord {
	return updatePendingRecord{
		gatewayID:       uuid.MustParse("1d91378f-7b96-4e6f-95b2-1304b728d28f"),
		fromVersion:     "1.0",
		toVersion:       "1.1",
		activeSHA256:    sha256.Sum256([]byte("active")),
		candidateSHA256: sha256.Sum256([]byte("candidate")),
		target:          "windows-amd64.exe",
	}
}

func TestUpdatePendingRecordIsExactFixedFormat(t *testing.T) {
	record := testPendingUpdateRecord()
	encoded, err := marshalUpdatePending(record)
	if err != nil {
		t.Fatal(err)
	}
	want := updatePendingHeader + "\n" +
		"gateway_id=1d91378f-7b96-4e6f-95b2-1304b728d28f\n" +
		"from_version=1.0\n" +
		"to_version=1.1\n" +
		"active_sha256=96879611650f80a81392a52e0db9b0237669087c4518e1c130e541a505e0eeef\n" +
		"candidate_sha256=dda18a0e21ae47c53b4309434cbc02ae8bf764fa83a6defbb719431242722aa7\n" +
		"target=windows-amd64.exe\n"
	if string(encoded) != want {
		t.Fatalf("pending record = %q, want %q", encoded, want)
	}
	parsed, err := parseUpdatePending(encoded)
	if err != nil || parsed != record {
		t.Fatalf("parsed record = %#v, err=%v", parsed, err)
	}
}

func TestUpdatePendingRecordRejectsAmbiguousState(t *testing.T) {
	valid, err := marshalUpdatePending(testPendingUpdateRecord())
	if err != nil {
		t.Fatal(err)
	}
	invalid := [][]byte{
		nil,
		append([]byte(nil), valid[:len(valid)-1]...),
		append(append([]byte(nil), valid...), '\n'),
		[]byte(strings.Replace(string(valid), "target=windows-amd64.exe", "unknown=value", 1)),
		[]byte(strings.Replace(string(valid), "to_version=1.1", "to_version=1.0", 1)),
		[]byte(strings.Replace(string(valid), "active_sha256=9", "active_sha256=A", 1)),
		[]byte(strings.Replace(string(valid), "gateway_id=1d91378f-7b96-4e6f-95b2-1304b728d28f", "gateway_id=not-a-uuid", 1)),
	}
	for index, encoded := range invalid {
		if _, err := parseUpdatePending(encoded); err == nil {
			t.Fatalf("invalid pending record %d was accepted: %q", index, encoded)
		}
	}
}

func TestUpdatePendingCreateOnlyRaceDoesNotAdoptAnotherPublisher(t *testing.T) {
	directory := t.TempDir()
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	record := testPendingUpdateRecord()
	encoded, err := marshalUpdatePending(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.updatePending, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveUpdatePending(paths, record); err == nil {
		t.Fatal("create-only pending publication adopted another publisher")
	}
	actual, err := os.ReadFile(paths.updatePending)
	if err != nil || string(actual) != string(encoded) {
		t.Fatalf("winning pending record changed: %q err=%v", actual, err)
	}
}

func TestUpdateStartupDecisionCoversCrashBoundaries(t *testing.T) {
	record := testPendingUpdateRecord()
	previous := record.activeSHA256
	unknown := sha256.Sum256([]byte("unknown"))
	tests := []struct {
		name      string
		record    *updatePendingRecord
		gatewayID uuid.UUID
		version   string
		target    string
		active    [sha256.Size]byte
		previous  *[sha256.Size]byte
		want      updateStartupAction
		wantError bool
	}{
		{"before previous publication", nil, record.gatewayID, "1.0", record.target, record.activeSHA256, nil, updateStartupCleanup, false},
		{"after previous before pending", nil, record.gatewayID, "1.0", record.target, record.activeSHA256, &previous, updateStartupCleanup, false},
		{"after pending before replacement", &record, record.gatewayID, "1.0", record.target, record.activeSHA256, &previous, updateStartupCleanup, false},
		{"after replacement", &record, record.gatewayID, "1.1", record.target, record.candidateSHA256, &previous, updateStartupRunCandidate, false},
		{"candidate corrupt with previous", &record, record.gatewayID, "1.1", record.target, unknown, &previous, updateStartupRollback, false},
		{"candidate corrupt without previous", &record, record.gatewayID, "1.1", record.target, unknown, nil, updateStartupCleanup, true},
		{"identity changed", &record, uuid.MustParse("ea4ee969-8484-48c4-9bcd-8de95815d9ab"), "1.1", record.target, record.candidateSHA256, &previous, updateStartupCleanup, true},
		{"target changed", &record, record.gatewayID, "1.1", "linux-amd64", record.candidateSHA256, &previous, updateStartupCleanup, true},
		{"source version inconsistent", &record, record.gatewayID, "9.9", record.target, record.activeSHA256, &previous, updateStartupCleanup, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			active := test.active
			action, err := decideUpdateStartupAction(
				test.record,
				test.gatewayID,
				test.version,
				test.target,
				&active,
				test.previous,
			)
			if action != test.want || (err != nil) != test.wantError {
				t.Fatalf("action=%d err=%v, want action=%d error=%v", action, err, test.want, test.wantError)
			}
		})
	}
	action, err := decideUpdateStartupAction(
		&record,
		record.gatewayID,
		"1.1",
		record.target,
		nil,
		&previous,
	)
	if err != nil || action != updateStartupRollback {
		t.Fatalf("unreadable active with exact previous = action %d, err %v", action, err)
	}
}

func TestPreparePreviousExecutableDoesNotDeleteSharedRecoveryState(t *testing.T) {
	directory := t.TempDir()
	active := filepath.Join(directory, "gateway")
	previous := active + ".previous"
	contents := []byte("known-good-gateway")
	if err := os.WriteFile(active, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(previous, contents, 0o700); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(contents)
	if got, err := preparePreviousExecutable(active, previous); err != nil || got != wantHash {
		t.Fatalf("matching shared previous was not reused: hash=%x err=%v", got, err)
	}
	if got, err := os.ReadFile(previous); err != nil || string(got) != string(contents) {
		t.Fatalf("shared previous changed: %q err=%v", got, err)
	}
	if _, err := os.Stat(previous + ".preparing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary preparation survived reuse: %v", err)
	}

	if err := os.WriteFile(previous+".preparing", []byte("other-process"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := preparePreviousExecutable(active, previous); err == nil {
		t.Fatal("concurrent previous preparation was accepted")
	}
	if got, err := os.ReadFile(previous); err != nil || string(got) != string(contents) {
		t.Fatalf("concurrent preparation deleted shared previous: %q err=%v", got, err)
	}
}

func TestLifecycleDiagnosticSanitizationIsBoundedAndSecretSafe(t *testing.T) {
	for _, secret := range []string{
		"https://example.invalid/download?token=secret",
		"Authorization: Bearer secret",
		"private key contents",
		"x-goog-signature=secret",
	} {
		if got := sanitizeLifecycleDetail(secret); got != "redacted detail" {
			t.Fatalf("sensitive diagnostic was not redacted: %q", got)
		}
	}
	long := strings.Repeat("safe ", 200)
	if got := sanitizeLifecycleDetail(long); len(got) > lifecycleStatusMaxDetail {
		t.Fatalf("diagnostic length = %d", len(got))
	}
	if got := sanitizeLifecycleDetail("line one\ncategory=forged"); strings.ContainsAny(got, "\r\n=") {
		t.Fatalf("diagnostic retained record delimiters: %q", got)
	}
}

func TestHashLifecycleFileRejectsNonRegularAndEmptyState(t *testing.T) {
	directory := t.TempDir()
	if _, _, err := hashLifecycleFile(directory); err == nil {
		t.Fatal("directory was accepted as a lifecycle file")
	}
	empty := filepath.Join(directory, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hashLifecycleFile(empty); err == nil {
		t.Fatal("empty lifecycle file was accepted")
	}
	file := filepath.Join(directory, "release")
	if err := os.WriteFile(file, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, size, err := hashLifecycleFile(file)
	if err != nil || size != int64(len("release")) || hash != sha256.Sum256([]byte("release")) {
		t.Fatalf("hash=%x size=%d err=%v", hash, size, err)
	}
}

func TestIdentityPathsContainOnlyBoundedLifecycleRecords(t *testing.T) {
	paths := newIdentityPaths(t.TempDir(), filepath.Join(t.TempDir(), "GatewayID"))
	if filepath.Base(paths.updatePending) != updatePendingFilename ||
		filepath.Base(paths.lifecycleStatus) != lifecycleStatusFilename ||
		filepath.Base(paths.removalPending) != removalPendingFilename {
		t.Fatalf("unexpected lifecycle paths: %#v", paths)
	}
}
