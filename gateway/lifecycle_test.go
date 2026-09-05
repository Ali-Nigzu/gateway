package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemovalMarkerRequiresExactTerminalValue(t *testing.T) {
	if err := validateRemovalMarker([]byte(removalMarkerContents)); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{nil, []byte("terminal-removal"), []byte("update\n"), []byte(removalMarkerContents + "extra")} {
		if err := validateRemovalMarker(invalid); err == nil {
			t.Fatalf("invalid marker %q was accepted", invalid)
		}
	}
}

func TestPrepareIdentityForFinalRemovalKeepsOnlyMarkerAndHelper(t *testing.T) {
	directory := t.TempDir()
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	if err := os.Mkdir(paths.workDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(paths.workDirectory, "removal-helper")
	for path, contents := range map[string]string{
		paths.gatewayID:      "gateway",
		paths.privateKey:     "key",
		paths.certificate:    "certificate",
		paths.removalPending: removalMarkerContents,
		helper:               "helper",
		filepath.Join(paths.workDirectory, "stale-candidate"): "stale",
	} {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepareIdentityForFinalRemoval(paths, helper); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name() != removalPendingFilename || entries[1].Name() != identityWorkDirectory {
		t.Fatalf("identity entries after preparation = %v", entries)
	}
	workEntries, err := os.ReadDir(paths.workDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(workEntries) != 1 || workEntries[0].Name() != filepath.Base(helper) {
		t.Fatalf("work entries after preparation = %v", workEntries)
	}
}

func TestPrepareIdentityForFinalRemovalRejectsHelperOutsideWorkDirectory(t *testing.T) {
	directory := t.TempDir()
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	if err := os.Mkdir(paths.workDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.removalPending, []byte(removalMarkerContents), 0o600); err != nil {
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
