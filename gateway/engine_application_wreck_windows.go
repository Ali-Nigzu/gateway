//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
)

const wreckTestMarkerFilename = "CAMOS-1.1-TEST.txt"

func runGatewayApplication(
	ctx context.Context,
	_ []deviceRecord,
	_ *runtimeCredentials,
) error {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	if err := prepareIdentityWorkDirectory(paths); err != nil {
		return err
	}
	marker := filepath.Join(paths.workDirectory, wreckTestMarkerFilename)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}
