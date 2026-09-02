//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsServiceName        = "camOSGateway"
	windowsServiceDisplayName = "camOS Gateway"
	windowsServiceDescription = "camOS camera gateway"
	windowsServiceAccount     = "LocalSystem"

	gatewayInstallDirectoryName = "camOS Gateway"
	gatewayExecutableFilename   = "camos-gateway.exe"
	gatewayFFmpegFilename       = "ffmpeg.exe"

	serviceTransitionTimeout = 2 * time.Minute
	serviceStatePollInterval = 250 * time.Millisecond
	recoveryResetPeriod      = 24 * 60 * 60
)

type windowsPackagePaths struct {
	directory   string
	executable  string
	ffmpeg      string
	credentials string
}

func installService(gatewayID uuid.UUID) error {
	paths, err := resolveWindowsPackagePaths()
	if err != nil {
		return err
	}
	if err := secureWindowsCredentialsIfPresent(paths.credentials); err != nil {
		return err
	}

	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("Service Control Manager unavailable: %w", err)
	}
	defer manager.Disconnect()

	service, err := manager.OpenService(windowsServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return installFirstWindowsService(manager, paths, gatewayID)
	}
	if err != nil {
		return fmt.Errorf("Gateway service inspection failed: %w", err)
	}
	defer service.Close()

	identitySame, err := storedGatewayIDMatches(gatewayID)
	if err != nil {
		return err
	}
	currentConfig, err := service.Config()
	if err != nil {
		return fmt.Errorf("Gateway service configuration read failed: %w", err)
	}
	expectedConfig := windowsServiceConfig(paths.executable)

	if !identitySame || serviceRuntimeConfigurationChanged(currentConfig, expectedConfig) {
		if err := stopWindowsService(service); err != nil {
			return err
		}
	}

	if err := saveGatewayID(gatewayID); err != nil {
		return err
	}
	if !windowsServiceConfigurationMatches(currentConfig, expectedConfig) {
		if err := service.UpdateConfig(expectedConfig); err != nil {
			return fmt.Errorf("Gateway service configuration failed: %w", err)
		}
		if len(currentConfig.Dependencies) != 0 {
			if err := clearWindowsServiceDependencies(service); err != nil {
				return err
			}
		}
	}
	if err := configureWindowsServiceRecovery(service); err != nil {
		return err
	}
	return ensureWindowsServiceRunning(service)
}

func installFirstWindowsService(
	manager *mgr.Mgr,
	paths windowsPackagePaths,
	gatewayID uuid.UUID,
) error {
	if err := installWindowsPackageIfAbsent(paths); err != nil {
		return err
	}
	if err := saveGatewayID(gatewayID); err != nil {
		return err
	}

	config := windowsServiceConfig(paths.executable)
	service, err := manager.CreateService(
		windowsServiceName,
		paths.executable,
		config,
		"service",
	)
	if err != nil {
		return fmt.Errorf("Gateway service installation failed: %w", err)
	}
	defer service.Close()

	if err := configureWindowsServiceRecovery(service); err != nil {
		return err
	}
	return ensureWindowsServiceRunning(service)
}

func resolveWindowsPackagePaths() (windowsPackagePaths, error) {
	programFiles, err := windows.KnownFolderPath(
		windows.FOLDERID_ProgramFiles,
		windows.KF_FLAG_DEFAULT,
	)
	if err != nil {
		return windowsPackagePaths{}, fmt.Errorf("Program Files path unavailable: %w", err)
	}
	directory := filepath.Join(programFiles, gatewayInstallDirectoryName)
	return windowsPackagePaths{
		directory:   directory,
		executable:  filepath.Join(directory, gatewayExecutableFilename),
		ffmpeg:      filepath.Join(directory, gatewayFFmpegFilename),
		credentials: filepath.Join(directory, serviceAccountFilename),
	}, nil
}

func installWindowsPackageIfAbsent(paths windowsPackagePaths) error {
	if err := os.MkdirAll(paths.directory, 0o755); err != nil {
		return fmt.Errorf("Gateway installation directory creation failed: %w", err)
	}

	executablePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("Gateway executable path unavailable: %w", err)
	}
	sourceDirectory := filepath.Dir(executablePath)
	packageFiles := []struct {
		source      string
		target      string
		mode        os.FileMode
		credentials bool
	}{
		{source: executablePath, target: paths.executable, mode: 0o755},
		{
			source: filepath.Join(sourceDirectory, gatewayFFmpegFilename),
			target: paths.ffmpeg,
			mode:   0o755,
		},
		{
			source:      filepath.Join(sourceDirectory, serviceAccountFilename),
			target:      paths.credentials,
			mode:        0o600,
			credentials: true,
		},
	}

	for _, packageFile := range packageFiles {
		if err := copyWindowsPackageFileIfAbsent(
			packageFile.source,
			packageFile.target,
			packageFile.mode,
			packageFile.credentials,
		); err != nil {
			return err
		}
	}
	return secureWindowsCredentialsIfPresent(paths.credentials)
}

func copyWindowsPackageFileIfAbsent(
	sourcePath string,
	targetPath string,
	mode os.FileMode,
	credentials bool,
) error {
	targetInfo, err := os.Lstat(targetPath)
	if err == nil {
		if !targetInfo.Mode().IsRegular() {
			return fmt.Errorf("installed package path is not a regular file: %s", targetPath)
		}
		return removeWindowsInstallingFile(targetPath + ".installing")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("installed package path inspection failed for %s: %w", targetPath, err)
	}

	temporaryPath := targetPath + ".installing"
	if err := removeWindowsInstallingFile(temporaryPath); err != nil {
		return err
	}

	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("package source unavailable at %s: %w", sourcePath, err)
	}
	defer source.Close()

	temporary, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("package temporary file creation failed for %s: %w", targetPath, err)
	}
	defer func() {
		temporary.Close()
		os.Remove(temporaryPath)
	}()

	if credentials {
		if err := applyWindowsCredentialDACL(temporaryPath); err != nil {
			return err
		}
	}
	if _, err := io.Copy(temporary, source); err != nil {
		return fmt.Errorf("package installation failed for %s: %w", targetPath, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("package sync failed for %s: %w", targetPath, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("package close failed for %s: %w", targetPath, err)
	}
	if err := moveWindowsPackageFileWithoutReplacement(temporaryPath, targetPath); err != nil {
		targetInfo, inspectionErr := os.Lstat(targetPath)
		if inspectionErr == nil {
			if !targetInfo.Mode().IsRegular() {
				return fmt.Errorf("installed package path is not a regular file: %s", targetPath)
			}
			return nil
		}
		if !errors.Is(inspectionErr, os.ErrNotExist) {
			return fmt.Errorf("installed package path inspection failed for %s: %w", targetPath, inspectionErr)
		}
		return fmt.Errorf("package publication failed for %s: %w", targetPath, err)
	}
	if credentials {
		return applyWindowsCredentialDACL(targetPath)
	}
	return nil
}

func removeWindowsInstallingFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("package temporary file cleanup failed for %s: %w", path, err)
	}
	return nil
}

func moveWindowsPackageFileWithoutReplacement(sourcePath, targetPath string) error {
	source, err := windows.UTF16PtrFromString(sourcePath)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		return err
	}
	return windows.MoveFile(source, target)
}

func secureWindowsCredentialsIfPresent(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("Gateway credentials inspection failed: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Gateway credentials path is not a regular file: %s", path)
	}
	return applyWindowsCredentialDACL(path)
}

func applyWindowsCredentialDACL(path string) error {
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return fmt.Errorf("Gateway credentials LocalSystem SID creation failed: %w", err)
	}
	administratorsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return fmt.Errorf("Gateway credentials Administrators SID creation failed: %w", err)
	}
	access := []windows.EXPLICIT_ACCESS{
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_USER,
				TrusteeValue: windows.TrusteeValueFromSID(systemSID),
			},
		},
		{
			AccessPermissions: windows.GENERIC_ALL,
			AccessMode:        windows.GRANT_ACCESS,
			Inheritance:       windows.NO_INHERITANCE,
			Trustee: windows.TRUSTEE{
				TrusteeForm:  windows.TRUSTEE_IS_SID,
				TrusteeType:  windows.TRUSTEE_IS_GROUP,
				TrusteeValue: windows.TrusteeValueFromSID(administratorsSID),
			},
		},
	}
	dacl, err := windows.ACLFromEntries(access, nil)
	if err != nil {
		return fmt.Errorf("Gateway credentials DACL creation failed: %w", err)
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
		return fmt.Errorf("Gateway credentials permissions failed: %w", err)
	}
	return nil
}

func windowsServiceConfig(executablePath string) mgr.Config {
	return mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartAutomatic,
		ErrorControl:     mgr.ErrorNormal,
		BinaryPathName:   windowsServiceCommandLine(executablePath),
		Dependencies:     nil,
		ServiceStartName: windowsServiceAccount,
		DisplayName:      windowsServiceDisplayName,
		Description:      windowsServiceDescription,
		SidType:          windows.SERVICE_SID_TYPE_NONE,
		DelayedAutoStart: false,
	}
}

func windowsServiceCommandLine(executablePath string) string {
	return syscall.EscapeArg(executablePath) + " service"
}

func windowsServiceConfigurationMatches(current, expected mgr.Config) bool {
	return current.ServiceType == expected.ServiceType &&
		current.StartType == expected.StartType &&
		current.ErrorControl == expected.ErrorControl &&
		current.BinaryPathName == expected.BinaryPathName &&
		len(current.Dependencies) == 0 &&
		strings.EqualFold(current.ServiceStartName, expected.ServiceStartName) &&
		current.DisplayName == expected.DisplayName &&
		current.Description == expected.Description &&
		current.SidType == expected.SidType &&
		current.DelayedAutoStart == expected.DelayedAutoStart
}

func serviceRuntimeConfigurationChanged(current, expected mgr.Config) bool {
	return current.ServiceType != expected.ServiceType ||
		current.BinaryPathName != expected.BinaryPathName ||
		!strings.EqualFold(current.ServiceStartName, expected.ServiceStartName)
}

func clearWindowsServiceDependencies(service *mgr.Service) error {
	emptyDependencies := [2]uint16{}
	if err := windows.ChangeServiceConfig(
		service.Handle,
		windows.SERVICE_NO_CHANGE,
		windows.SERVICE_NO_CHANGE,
		windows.SERVICE_NO_CHANGE,
		nil,
		nil,
		nil,
		&emptyDependencies[0],
		nil,
		nil,
		nil,
	); err != nil {
		return fmt.Errorf("Gateway service dependency removal failed: %w", err)
	}
	return nil
}

func configureWindowsServiceRecovery(service *mgr.Service) error {
	actions := []mgr.RecoveryAction{{
		Type:  mgr.ServiceRestart,
		Delay: 30 * time.Second,
	}}
	if err := service.SetRecoveryActions(actions, recoveryResetPeriod); err != nil {
		return fmt.Errorf("Gateway service crash recovery configuration failed: %w", err)
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(false); err != nil {
		return fmt.Errorf("Gateway service crash recovery policy failed: %w", err)
	}
	return nil
}

func stopWindowsService(service *mgr.Service) error {
	deadline := time.Now().Add(serviceTransitionTimeout)
	stopRequested := false
	for {
		status, err := service.Query()
		if err != nil {
			return fmt.Errorf("Gateway service state query failed: %w", err)
		}
		if status.State == svc.Stopped {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Gateway service stop timed out in state %d", status.State)
		}

		if status.State != svc.StopPending && !stopRequested {
			_, err := service.Control(svc.Stop)
			switch {
			case err == nil:
				stopRequested = true
			case errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE):
				return nil
			case errors.Is(err, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL):
			default:
				return fmt.Errorf("Gateway service stop failed: %w", err)
			}
		}
		time.Sleep(serviceStatePollInterval)
	}
}

func ensureWindowsServiceRunning(service *mgr.Service) error {
	deadline := time.Now().Add(serviceTransitionTimeout)
	startRequested := false
	continueRequested := false
	for {
		status, err := service.Query()
		if err != nil {
			return fmt.Errorf("Gateway service state query failed: %w", err)
		}
		if status.State == svc.Running {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Gateway service start timed out in state %d", status.State)
		}

		switch status.State {
		case svc.Stopped:
			if startRequested {
				return errors.New("Gateway service did not remain running")
			}
			if err := service.Start(); err != nil &&
				!errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
				return fmt.Errorf("Gateway service start failed: %w", err)
			}
			startRequested = true

		case svc.Paused:
			if !continueRequested {
				if _, err := service.Control(svc.Continue); err != nil {
					return fmt.Errorf("Gateway service continue failed: %w", err)
				}
				continueRequested = true
			}
		}

		time.Sleep(serviceStatePollInterval)
	}
}
