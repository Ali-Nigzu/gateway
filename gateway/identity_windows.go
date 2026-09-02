//go:build windows

package main

import (
	"errors"
	"fmt"
	"syscall"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	registryPath           = `SOFTWARE\camOS\Gateway`
	registryGatewayIDValue = "GatewayID"
	legacySiteIDValue      = "SiteID"
)

var procRegFlushKey = windows.NewLazySystemDLL("advapi32.dll").NewProc("RegFlushKey")

func saveGatewayID(gatewayID uuid.UUID) error {
	key, _, err := registry.CreateKey(
		registry.LOCAL_MACHINE,
		registryPath,
		registry.SET_VALUE|registry.QUERY_VALUE|registry.WOW64_64KEY,
	)
	if err != nil {
		return fmt.Errorf("GatewayID write failed: %w", err)
	}
	defer key.Close()

	if err := key.SetStringValue(registryGatewayIDValue, gatewayID.String()); err != nil {
		return fmt.Errorf("GatewayID write failed: %w", err)
	}
	if err := key.DeleteValue(legacySiteIDValue); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return fmt.Errorf("legacy SiteID removal failed: %w", err)
	}
	status, _, _ := procRegFlushKey.Call(uintptr(key))
	if status != 0 {
		return fmt.Errorf("GatewayID flush failed: %w", syscall.Errno(status))
	}
	return nil
}

func loadGatewayID() (uuid.UUID, error) {
	encoded, present, err := readStoredGatewayID()
	if err != nil {
		return uuid.Nil, fmt.Errorf(
			"GatewayID unavailable; run commission <gateway_id> as administrator: %w",
			err,
		)
	}
	if !present {
		return uuid.Nil, errors.New(
			"GatewayID unavailable; run commission <gateway_id> as administrator",
		)
	}

	gatewayID, err := uuid.Parse(encoded)
	if err != nil {
		return uuid.Nil, errors.New(
			"GatewayID unavailable; run commission <gateway_id> as administrator",
		)
	}
	return gatewayID, nil
}

func storedGatewayIDMatches(gatewayID uuid.UUID) (bool, error) {
	encoded, present, err := readStoredGatewayID()
	if err != nil {
		return false, fmt.Errorf("GatewayID read failed: %w", err)
	}
	if !present {
		return false, nil
	}

	storedGatewayID, err := uuid.Parse(encoded)
	if err != nil {
		return false, nil
	}
	return storedGatewayID == gatewayID, nil
}

func readStoredGatewayID() (string, bool, error) {
	key, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		registryPath,
		registry.QUERY_VALUE|registry.WOW64_64KEY,
	)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer key.Close()

	value, _, err := key.GetStringValue(registryGatewayIDValue)
	if errors.Is(err, registry.ErrNotExist) {
		return "", false, nil
	}
	if errors.Is(err, registry.ErrUnexpectedType) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}
