//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCopyWindowsPackageFileIfAbsent(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(source, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyWindowsPackageFileIfAbsent(source, target, 0o600); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, target, "first")

	if err := os.WriteFile(source, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyWindowsPackageFileIfAbsent(source, target, 0o600); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, target, "first")
}

func TestCopyWindowsPackageFileRecoversInterruptedFirstInstall(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(source, []byte("complete"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+".installing", []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := copyWindowsPackageFileIfAbsent(source, target, 0o600); err != nil {
		t.Fatal(err)
	}
	assertFileContents(t, target, "complete")
	if _, err := os.Stat(target + ".installing"); !os.IsNotExist(err) {
		t.Fatalf("installing file remains: %v", err)
	}
}

func TestMoveWindowsPackageFileDoesNotReplace(t *testing.T) {
	directory := t.TempDir()
	source := filepath.Join(directory, "source")
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(source, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := moveWindowsPackageFileWithoutReplacement(source, target); err == nil {
		t.Fatal("MoveFile unexpectedly replaced an existing target")
	}
	assertFileContents(t, target, "existing")
	assertFileContents(t, source, "new")
}

func assertFileContents(t *testing.T, path, expected string) {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != expected {
		t.Fatalf("%s contains %q, want %q", path, contents, expected)
	}
}
