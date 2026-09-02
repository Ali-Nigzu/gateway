//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

const (
	gatewayIdentityDirectory = "/Library/Application Support/camOS Gateway"
	gatewayIdentityPath      = gatewayIdentityDirectory + "/GatewayID"
)

func saveGatewayID(gatewayID uuid.UUID) error {
	if err := os.MkdirAll(gatewayIdentityDirectory, 0o700); err != nil {
		return fmt.Errorf("GatewayID directory creation failed: %w", err)
	}
	if err := os.Chown(gatewayIdentityDirectory, 0, 0); err != nil {
		return fmt.Errorf("GatewayID directory ownership failed: %w", err)
	}
	if err := os.Chmod(gatewayIdentityDirectory, 0o700); err != nil {
		return fmt.Errorf("GatewayID directory permissions failed: %w", err)
	}

	temporaryPath := gatewayIdentityPath + ".installing"
	if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("GatewayID temporary file cleanup failed: %w", err)
	}
	temporary, err := os.OpenFile(
		temporaryPath,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("GatewayID write failed: %w", err)
	}
	keepTemporary := true
	defer func() {
		if keepTemporary {
			os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chown(0, 0); err != nil {
		temporary.Close()
		return fmt.Errorf("GatewayID ownership failed: %w", err)
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("GatewayID permissions failed: %w", err)
	}
	if _, err := temporary.WriteString(gatewayID.String()); err != nil {
		temporary.Close()
		return fmt.Errorf("GatewayID write failed: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("GatewayID sync failed: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("GatewayID close failed: %w", err)
	}
	if err := os.Rename(temporaryPath, gatewayIdentityPath); err != nil {
		return fmt.Errorf("GatewayID replacement failed: %w", err)
	}
	keepTemporary = false

	directory, err := os.Open(filepath.Dir(gatewayIdentityPath))
	if err != nil {
		return fmt.Errorf("GatewayID directory sync failed: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("GatewayID directory sync failed: %w", err)
	}
	return nil
}

func loadGatewayID() (uuid.UUID, error) {
	encoded, err := os.ReadFile(gatewayIdentityPath)
	if err != nil {
		return uuid.Nil, fmt.Errorf("GatewayID unavailable; run commission <gateway_id> as root: %w", err)
	}

	gatewayID, err := uuid.Parse(string(encoded))
	if err != nil {
		return uuid.Nil, fmt.Errorf("GatewayID unavailable; run commission <gateway_id> as root")
	}
	return gatewayID, nil
}

func secureGatewayIdentity() error {
	if err := os.Chown(gatewayIdentityDirectory, 0, 0); err != nil {
		return fmt.Errorf("GatewayID directory ownership failed: %w", err)
	}
	if err := os.Chmod(gatewayIdentityDirectory, 0o700); err != nil {
		return fmt.Errorf("GatewayID directory permissions failed: %w", err)
	}
	if err := os.Chown(gatewayIdentityPath, 0, 0); err != nil {
		return fmt.Errorf("GatewayID ownership failed: %w", err)
	}
	if err := os.Chmod(gatewayIdentityPath, 0o600); err != nil {
		return fmt.Errorf("GatewayID permissions failed: %w", err)
	}
	return nil
}
