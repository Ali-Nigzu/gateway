package main

import (
	"errors"
	"os"
	"path/filepath"
)

const removalMarkerContents = "terminal-removal\n"

func markGatewayRemovalPending() error {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	if err := atomicWriteIdentityFile(
		paths,
		paths.removalPending,
		[]byte(removalMarkerContents),
		true,
	); err != nil {
		return errors.New("terminal removal marker persistence failed")
	}
	return nil
}

func gatewayRemovalPending() (bool, error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return false, err
	}
	contents, err := os.ReadFile(paths.removalPending)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("terminal removal marker is unavailable")
	}
	if err := validateRemovalMarker(contents); err != nil {
		return false, err
	}
	return true, nil
}

func validateRemovalMarker(contents []byte) error {
	if string(contents) != removalMarkerContents {
		return errors.New("terminal removal marker is invalid")
	}
	return nil
}

// prepareIdentityForFinalRemoval erases durable credentials while retaining
// only the authorization marker and the currently running transient helper.
// The native supervisor can therefore retry every fallible preparation step;
// the helper and marker disappear together only in the final directory remove.
func prepareIdentityForFinalRemoval(paths identityPaths, helperPath string) error {
	directory := filepath.Clean(paths.directory)
	workDirectory := filepath.Clean(paths.workDirectory)
	markerPath := filepath.Clean(paths.removalPending)
	helperPath = filepath.Clean(helperPath)
	if directory == "." || directory == string(os.PathSeparator) ||
		filepath.Clean(filepath.Dir(workDirectory)) != directory ||
		filepath.Clean(filepath.Dir(markerPath)) != directory ||
		filepath.Clean(filepath.Dir(helperPath)) != workDirectory {
		return errors.New("terminal removal paths are invalid")
	}
	helperInfo, err := os.Lstat(helperPath)
	if err != nil || !helperInfo.Mode().IsRegular() {
		return errors.New("terminal removal helper is invalid")
	}
	marker, err := os.ReadFile(markerPath)
	if err != nil || validateRemovalMarker(marker) != nil {
		return errors.New("terminal removal marker is invalid")
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		return errors.New("Gateway identity inspection failed")
	}
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		if filepath.Clean(path) == markerPath || filepath.Clean(path) == workDirectory {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return errors.New("Gateway identity preparation failed")
		}
	}

	workEntries, err := os.ReadDir(workDirectory)
	if err != nil {
		return errors.New("Gateway lifecycle state inspection failed")
	}
	for _, entry := range workEntries {
		path := filepath.Join(workDirectory, entry.Name())
		if filepath.Clean(path) == helperPath {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return errors.New("Gateway lifecycle state preparation failed")
		}
	}
	if err := syncIdentityDirectory(workDirectory); err != nil {
		return err
	}
	return syncIdentityDirectory(directory)
}
