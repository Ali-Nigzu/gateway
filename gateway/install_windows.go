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
	directory  string
	executable string
	ffmpeg     string
}

func installService(prepared preparedCommission, now time.Time) error {
	releaseLifecycle, err := acquireGatewayLifecycleLock(lifecycleOperationLockWait)
	if err != nil {
		return err
	}
	lockHeld := true
	defer func() {
		if lockHeld {
			releaseLifecycle()
		}
	}()
	identityPaths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	if err := ensureCommissioningAllowed(identityPaths); err != nil {
		return err
	}
	if err := validatePreparedCommissionServiceInstall(prepared, now); err != nil {
		return err
	}
	if err := validateEmbeddedRelease(); err != nil {
		return err
	}
	paths, err := resolveWindowsPackagePaths()
	if err != nil {
		return err
	}
	// Repair package bytes even when an interrupted terminal removal left the
	// named SCM service behind. The new commissioning identity is committed
	// before this call, and the commissioning guard proves no old marker/helper
	// tombstone remains before any canonical pathname can be reused.
	if err := installWindowsPackageIfAbsent(paths); err != nil {
		return err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("Service Control Manager unavailable: %w", err)
	}
	defer manager.Disconnect()

	service, err := manager.OpenService(windowsServiceName)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		service, err = createFirstWindowsService(manager, paths)
	}
	if err != nil {
		return fmt.Errorf("Gateway service inspection failed: %w", err)
	}
	defer service.Close()

	if service != nil {
		currentConfig, err := service.Config()
		if err != nil {
			return fmt.Errorf("Gateway service configuration read failed: %w", err)
		}
		expectedConfig := windowsServiceConfig(paths.executable)

		if serviceRuntimeConfigurationChanged(currentConfig, expectedConfig) {
			if err := stopWindowsService(service); err != nil {
				return err
			}
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
	}
	if err := ensureCommissioningAllowed(identityPaths); err != nil {
		return err
	}
	if err := requestWindowsServiceStart(service); err != nil {
		return err
	}
	// Release once SCM has accepted the start/continue request. The service can
	// then take the same lock for startup recovery while this caller only waits.
	releaseLifecycle()
	lockHeld = false
	return waitForWindowsServiceRunningAfterRequest(service)
}

func createFirstWindowsService(
	manager *mgr.Mgr,
	paths windowsPackagePaths,
) (*mgr.Service, error) {
	config := windowsServiceConfig(paths.executable)
	service, err := manager.CreateService(
		windowsServiceName,
		paths.executable,
		config,
		"service",
	)
	if err != nil {
		return nil, fmt.Errorf("Gateway service installation failed: %w", err)
	}

	return service, nil
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
		directory:  directory,
		executable: filepath.Join(directory, gatewayExecutableFilename),
		ffmpeg:     filepath.Join(directory, gatewayFFmpegFilename),
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
	packageFiles := []struct {
		source string
		target string
		mode   os.FileMode
	}{
		{source: executablePath, target: paths.executable, mode: 0o755},
	}

	for _, packageFile := range packageFiles {
		if err := copyWindowsPackageFileIfAbsent(
			packageFile.source,
			packageFile.target,
			packageFile.mode,
		); err != nil {
			return err
		}
	}
	return ensureEmbeddedFFmpeg(paths.ffmpeg)
}

func copyWindowsPackageFileIfAbsent(
	sourcePath string,
	targetPath string,
	mode os.FileMode,
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
	if err := service.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
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

func requestWindowsServiceStart(service *mgr.Service) error {
	if service == nil {
		return errors.New("Gateway service is unavailable")
	}
	status, err := service.Query()
	if err != nil {
		return fmt.Errorf("Gateway service state query failed: %w", err)
	}
	switch status.State {
	case svc.Running, svc.StartPending, svc.ContinuePending:
		return nil
	case svc.Stopped:
		if err := service.Start(); err != nil &&
			!errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			return fmt.Errorf("Gateway service start failed: %w", err)
		}
		return nil
	case svc.Paused:
		if _, err := service.Control(svc.Continue); err != nil {
			return fmt.Errorf("Gateway service continue failed: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("Gateway service cannot start from state %d", status.State)
	}
}

func waitForWindowsServiceRunningAfterRequest(service *mgr.Service) error {
	deadline := time.Now().Add(serviceTransitionTimeout)
	for {
		status, err := service.Query()
		if err != nil {
			return fmt.Errorf("Gateway service state query failed: %w", err)
		}
		if status.State == svc.Running {
			return nil
		}
		if status.State == svc.Stopped {
			return errors.New("Gateway service did not remain running")
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Gateway service start timed out in state %d", status.State)
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
