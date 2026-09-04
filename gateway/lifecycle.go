package main

import (
	"errors"
	"os"
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
