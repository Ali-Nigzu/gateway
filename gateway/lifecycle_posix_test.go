//go:build darwin || linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPOSIXLifecycleLockUsesStableParentBeforeCommissioningCreatesIdentity(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "identity")
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	lockPath, err := posixLifecycleLockPath(paths)
	if err != nil {
		t.Fatal(err)
	}
	if lockPath != filepath.Dir(directory) {
		t.Fatalf("lifecycle lock path = %q, want %q", lockPath, filepath.Dir(directory))
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock-path selection created the identity directory: %v", err)
	}
}

func TestValidatePOSIXLockPathRejectsReplacedNamedInode(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "lock-target")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if err := validatePOSIXLockPath(opened, path); err != nil {
		t.Fatalf("current named inode was rejected: %v", err)
	}
	if err := os.Rename(path, path+".retired"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePOSIXLockPath(opened, path); err == nil {
		t.Fatal("stale open inode was accepted after the canonical name was replaced")
	}
}

func TestPOSIXIdentityStateWriteDoesNotRecreateRemovedRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "removed-identity")
	paths := newIdentityPaths(root, filepath.Join(root, "GatewayID"))
	if err := atomicWriteIdentityFile(
		paths,
		paths.lifecycleStatus,
		[]byte("must-not-persist"),
		true,
	); err == nil {
		t.Fatal("runtime identity write succeeded after terminal root removal")
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime identity write recreated the removed root: %v", err)
	}
}

func TestDecidePOSIXRemovalHelperPreparationIsMonotonic(t *testing.T) {
	tests := []struct {
		name           string
		marker         bool
		gatewayID      bool
		native         bool
		wantPrepare    bool
		wantErr        bool
		wantNativeRead bool
	}{
		{name: "initial committed removal", marker: true, gatewayID: true, wantPrepare: true},
		{name: "repair before native setup", marker: true, wantPrepare: true, wantNativeRead: true},
		{name: "native finalizer owns no-ID phase", marker: true, native: true, wantNativeRead: true},
		{name: "completed removal", wantPrepare: false},
		{name: "GatewayID without authority", gatewayID: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			nativeRead := false
			prepare, err := decidePOSIXRemovalHelperPreparation(
				test.marker,
				test.gatewayID,
				func() (bool, error) {
					nativeRead = true
					return test.native, nil
				},
			)
			if (err != nil) != test.wantErr || prepare != test.wantPrepare {
				t.Fatalf("prepare=%v err=%v", prepare, err)
			}
			if nativeRead != test.wantNativeRead {
				t.Fatalf("native state read=%v, want %v", nativeRead, test.wantNativeRead)
			}
		})
	}

	if _, err := decidePOSIXRemovalHelperPreparation(
		true,
		false,
		func() (bool, error) { return false, errors.New("ambiguous") },
	); err == nil {
		t.Fatal("ambiguous native finalizer state was accepted")
	}
}

func TestPOSIXRemovalFinalizerRequiresAtomicTombstoneHandoff(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "identity with spaces")
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	helper := filepath.Join(paths.workDirectory, "removal-helper")
	finalizer, err := posixRemovalFinalizerPrefix(paths, helper)
	if err != nil {
		t.Fatal(err)
	}
	tombstone, err := identityRemovalTombstonePath(paths)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{
		"internal-remove",
		paths.removalPending,
		helper,
		"test ! -e " + quotePOSIXShellArgument(paths.directory),
		"test ! -L " + quotePOSIXShellArgument(paths.directory),
		"test ! -L " + quotePOSIXShellArgument(tombstone),
		"/bin/rm -rf -- " + quotePOSIXShellArgument(tombstone),
	} {
		if !strings.Contains(finalizer, required) {
			t.Fatalf("finalizer is missing %q", required)
		}
	}
	if strings.Contains(finalizer, "/bin/rm -rf -- "+quotePOSIXShellArgument(paths.directory)) {
		t.Fatal("stable wrapper can recursively delete the canonical identity generation")
	}
	if strings.Index(finalizer, "internal-remove") >
		strings.Index(finalizer, "/bin/rm -rf -- "+quotePOSIXShellArgument(tombstone)) {
		t.Fatal("stable wrapper can remove the tombstone before the validating helper succeeds")
	}
}

func TestPOSIXRemovalTombstoneAlwaysBlocksNormalLifecycle(t *testing.T) {
	paths := newIdentityPaths(
		filepath.Join(t.TempDir(), "identity"),
		filepath.Join(t.TempDir(), "unused-GatewayID"),
	)
	if present, err := posixRemovalTombstonePresent(paths); err != nil || present {
		t.Fatalf("absent tombstone = present %v, err %v", present, err)
	}
	tombstone, err := identityRemovalTombstonePath(paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(tombstone, 0o700); err != nil {
		t.Fatal(err)
	}
	if present, err := posixRemovalTombstonePresent(paths); err != nil || !present {
		t.Fatalf("present tombstone = present %v, err %v", present, err)
	}
}

func TestStageIdentityDirectoryForRemovalAtomicallyRetiresGeneration(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "identity")
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	if err := os.MkdirAll(paths.workDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.gatewayID, []byte(testRemovalGatewayID.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.removalPending, mustRemovalMarker(t, testRemovalGatewayID), 0o600); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(paths.workDirectory, "removal-helper")
	if err := os.WriteFile(helper, []byte("helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(directory, "private-material")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	openedGeneration, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer openedGeneration.Close()

	if err := stageIdentityDirectoryForRemoval(paths, helper); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canonical identity generation remains after staging: %v", err)
	}
	tombstone, err := identityRemovalTombstonePath(paths)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(tombstone)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("staged tombstone is invalid: %v", err)
	}
	for _, retained := range []string{
		filepath.Join(tombstone, removalPendingFilename),
		filepath.Join(tombstone, identityWorkDirectory, filepath.Base(helper)),
	} {
		if info, err := os.Lstat(retained); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("required removal authority %q was not retained: %v", retained, err)
		}
	}
	for _, removed := range []string{
		filepath.Join(tombstone, filepath.Base(paths.gatewayID)),
		filepath.Join(tombstone, filepath.Base(secret)),
	} {
		if _, err := os.Lstat(removed); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("sensitive identity state %q remains: %v", removed, err)
		}
	}
	if err := validatePOSIXLockPath(openedGeneration, directory); err == nil {
		t.Fatal("a waiter on the retired identity generation remained authoritative")
	}
}
