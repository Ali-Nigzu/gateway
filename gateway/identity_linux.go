//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

const (
	gatewayIdentityDirectory = "/var/lib/camos-gateway"
	gatewayIdentityPath      = gatewayIdentityDirectory + "/GatewayID"
)

func resolveIdentityPaths() (identityPaths, error) {
	return newIdentityPaths(gatewayIdentityDirectory, gatewayIdentityPath), nil
}

func requireCommissionPrivileges() error {
	if os.Geteuid() != 0 {
		return errors.New("commissioning must be run as root")
	}
	return nil
}

func prepareIdentityDirectory(paths identityPaths) error {
	if err := os.MkdirAll(paths.directory, 0o700); err != nil {
		return errors.New("Gateway identity directory creation failed")
	}
	info, err := os.Lstat(paths.directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Gateway identity directory is invalid")
	}
	if err := os.Chown(paths.directory, 0, 0); err != nil {
		return errors.New("Gateway identity directory ownership failed")
	}
	if err := os.Chmod(paths.directory, 0o700); err != nil {
		return errors.New("Gateway identity directory permissions failed")
	}
	return nil
}

func secureIdentityFile(path string) error {
	if err := os.Chown(path, 0, 0); err != nil {
		return errors.New("Gateway identity file ownership failed")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return errors.New("Gateway identity file permissions failed")
	}
	return nil
}

func publishIdentityFile(sourcePath, targetPath string, replace bool) (bool, error) {
	if replace {
		if err := os.Rename(sourcePath, targetPath); err != nil {
			return false, errors.New("identity state publication failed")
		}
		return true, nil
	}
	if err := os.Link(sourcePath, targetPath); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, errors.New("identity state publication failed")
	}
	if err := os.Remove(sourcePath); err != nil {
		return false, errors.New("identity state temporary file cleanup failed")
	}
	return true, nil
}

func syncIdentityDirectory(path string) error {
	directory, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("Gateway identity directory sync failed")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errors.New("Gateway identity directory sync failed")
	}
	return nil
}

func saveGatewayID(gatewayID uuid.UUID) error {
	if gatewayID == uuid.Nil {
		return errors.New("GatewayID write failed")
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	if err := atomicWriteIdentityFile(paths, paths.gatewayID, []byte(gatewayID.String()), true); err != nil {
		return fmt.Errorf("GatewayID write failed: %w", err)
	}
	return nil
}

func loadGatewayID() (uuid.UUID, error) {
	encoded, err := os.ReadFile(gatewayIdentityPath)
	if err != nil {
		return uuid.Nil, fmt.Errorf(
			"GatewayID unavailable; run commission <commission_id> as root: %w",
			err,
		)
	}

	gatewayID, err := uuid.Parse(string(encoded))
	if err != nil || gatewayID == uuid.Nil || string(encoded) != gatewayID.String() {
		return uuid.Nil, errors.New("GatewayID unavailable; run commission <commission_id> as root")
	}
	return gatewayID, nil
}

func gatewayIDCommitPresent() (bool, error) {
	_, err := os.Lstat(gatewayIdentityPath)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, errors.New("GatewayID inspection failed")
}

func secureGatewayIdentity() error {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	if err := prepareIdentityDirectory(paths); err != nil {
		return err
	}
	for _, path := range []string{
		paths.gatewayID,
		paths.privateKey,
		paths.certificate,
		paths.certificateConfig,
		paths.commissionHash,
	} {
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return errors.New("Gateway identity file inspection failed")
		}
		if err := secureIdentityFile(path); err != nil {
			return err
		}
	}
	return nil
}

func deleteGatewayID() error {
	if err := os.Remove(gatewayIdentityPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("GatewayID removal failed")
	}
	return syncIdentityDirectory(filepath.Dir(gatewayIdentityPath))
}
