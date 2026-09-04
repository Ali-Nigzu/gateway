//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	registryPath           = `SOFTWARE\camOS\Gateway`
	registryGatewayIDValue = "GatewayID"
	legacySiteIDValue      = "SiteID"

	windowsIdentityDirectoryName = "camOS Gateway"
)

var procRegFlushKey = windows.NewLazySystemDLL("advapi32.dll").NewProc("RegFlushKey")

func resolveIdentityPaths() (identityPaths, error) {
	programData, err := windows.KnownFolderPath(
		windows.FOLDERID_ProgramData,
		windows.KF_FLAG_DEFAULT,
	)
	if err != nil {
		return identityPaths{}, errors.New("ProgramData path unavailable")
	}
	directory := filepath.Join(programData, windowsIdentityDirectoryName)
	return newIdentityPaths(directory, ""), nil
}

func requireCommissionPrivileges() error {
	if !windows.GetCurrentProcessToken().IsElevated() {
		return errors.New("commissioning must be run as administrator")
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
	if err := applyWindowsIdentityDACL(
		paths.directory,
		windows.SUB_CONTAINERS_AND_OBJECTS_INHERIT,
	); err != nil {
		return err
	}
	return nil
}

func secureIdentityFile(path string) error {
	return applyWindowsIdentityDACL(path, windows.NO_INHERITANCE)
}

func applyWindowsIdentityDACL(path string, inheritance uint32) error {
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return errors.New("Gateway identity LocalSystem SID creation failed")
	}
	administratorsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return errors.New("Gateway identity Administrators SID creation failed")
	}
	access := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(systemSID),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       inheritance,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(administratorsSID),
			},
		},
	}
	dacl, err := windows.ACLFromEntries(access, nil)
	if err != nil {
		return errors.New("Gateway identity DACL creation failed")
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return errors.New("Gateway identity permissions failed")
	}
	return nil
}

func publishIdentityFile(sourcePath, targetPath string, replace bool) (bool, error) {
	source, err := windows.UTF16PtrFromString(sourcePath)
	if err != nil {
		return false, errors.New("identity state path is invalid")
	}
	target, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		return false, errors.New("identity state path is invalid")
	}
	if replace {
		err = windows.MoveFileEx(
			source,
			target,
			windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
		)
	} else {
		err = windows.MoveFile(source, target)
	}
	if !replace && (errors.Is(err, windows.ERROR_ALREADY_EXISTS) ||
		errors.Is(err, windows.ERROR_FILE_EXISTS)) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("identity state publication failed")
	}
	return true, nil
}

// MoveFileEx with MOVEFILE_WRITE_THROUGH makes the publication durable on
// Windows. Windows directories cannot be opened and synced like Unix ones.
func syncIdentityDirectory(_ string) error {
	return nil
}

func saveGatewayID(gatewayID uuid.UUID) error {
	if gatewayID == uuid.Nil {
		return errors.New("GatewayID write failed")
	}
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
			"GatewayID unavailable; run commission <commission_id> as administrator: %w",
			err,
		)
	}
	if !present {
		return uuid.Nil, errors.New(
			"GatewayID unavailable; run commission <commission_id> as administrator",
		)
	}

	gatewayID, err := uuid.Parse(encoded)
	if err != nil || gatewayID == uuid.Nil || encoded != gatewayID.String() {
		return uuid.Nil, errors.New(
			"GatewayID unavailable; run commission <commission_id> as administrator",
		)
	}
	return gatewayID, nil
}

func gatewayIDCommitPresent() (bool, error) {
	_, present, err := readStoredGatewayID()
	return present, err
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
	if errors.Is(err, registry.ErrNotExist) || errors.Is(err, registry.ErrUnexpectedType) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func deleteGatewayID() error {
	err := registry.DeleteKey(registry.LOCAL_MACHINE, registryPath)
	if errors.Is(err, registry.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("GatewayID removal failed")
	}
	return nil
}
