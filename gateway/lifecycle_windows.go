//go:build windows

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsInternalUpdateCommand  = "internal-update"
	windowsInternalRestartCommand = "internal-restart"
	windowsInternalRemoveCommand  = "internal-remove"
	windowsCandidateFilename      = "camos-gateway.candidate.exe"
)

func replaceFileAtomically(sourcePath, targetPath string) error {
	source, err := windows.UTF16PtrFromString(sourcePath)
	if err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(targetPath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		source,
		target,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}

func secureInstalledExecutable(path string) error {
	return os.Chmod(path, 0o755)
}

// MoveFileEx with MOVEFILE_WRITE_THROUGH makes publication durable on
// Windows. Windows directories cannot be opened and synced like Unix ones.
func syncParentDirectory(string) error { return nil }

func installedExecutablePath() (string, error) {
	paths, err := resolveWindowsPackagePaths()
	return paths.executable, err
}

func installedFFmpegPath() (string, error) {
	paths, err := resolveWindowsPackagePaths()
	return paths.ffmpeg, err
}

func candidateExecutablePath() (string, error) {
	paths, err := resolveWindowsPackagePaths()
	if err != nil {
		return "", err
	}
	return filepath.Join(paths.directory, windowsCandidateFilename), nil
}

func applyGatewayUpdate(candidatePath string) error {
	expectedCandidate, err := candidateExecutablePath()
	if err != nil {
		return err
	}
	if !sameWindowsPath(candidatePath, expectedCandidate) {
		return errors.New("Gateway candidate path is invalid")
	}
	if err := requireRegularFile(candidatePath); err != nil {
		return fmt.Errorf("Gateway candidate is invalid: %w", err)
	}
	helper, err := prepareWindowsLifecycleHelper("update")
	if err != nil {
		return err
	}
	return launchWindowsLifecycleHelper(helper, windowsInternalUpdateCommand)
}

func beginGatewayRestart() error {
	helper, err := prepareWindowsLifecycleHelper("restart")
	if err != nil {
		return err
	}
	return launchWindowsLifecycleHelper(helper, windowsInternalRestartCommand)
}

func beginGatewayRemoval() error {
	if err := markGatewayRemovalPending(); err != nil {
		return err
	}
	helper, err := prepareWindowsLifecycleHelper("remove")
	if err != nil {
		return err
	}
	return launchWindowsLifecycleHelper(helper, windowsInternalRemoveCommand)
}

func handleInternalPlatformCommand(arguments []string) (bool, error) {
	if len(arguments) == 0 {
		return false, nil
	}
	command := arguments[0]
	if command != windowsInternalUpdateCommand &&
		command != windowsInternalRestartCommand &&
		command != windowsInternalRemoveCommand {
		return false, nil
	}
	if len(arguments) != 2 {
		return true, errors.New("invalid internal lifecycle invocation")
	}
	parent, err := strconv.ParseUint(arguments[1], 10, 32)
	if err != nil || parent == 0 {
		return true, errors.New("invalid internal lifecycle invocation")
	}
	if err := waitForWindowsProcess(uint32(parent), serviceTransitionTimeout); err != nil {
		return true, err
	}

	switch command {
	case windowsInternalUpdateCommand:
		err = runWindowsUpdateHelper()
	case windowsInternalRestartCommand:
		err = startWindowsGatewayService()
	case windowsInternalRemoveCommand:
		err = runWindowsRemovalHelper()
	}
	if command != windowsInternalRemoveCommand {
		removeWindowsLifecycleHelper()
	}
	return true, err
}

func prepareWindowsLifecycleHelper(kind string) (string, error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return "", err
	}
	if err := prepareIdentityWorkDirectory(paths); err != nil {
		return "", err
	}
	source, err := os.Executable()
	if err != nil {
		return "", errors.New("Gateway executable path unavailable")
	}
	helper := filepath.Join(
		paths.workDirectory,
		fmt.Sprintf("camos-gateway-%s-%d.exe", kind, os.Getpid()),
	)
	if err := copyWindowsLifecycleFile(source, helper); err != nil {
		return "", err
	}
	return helper, nil
}

func copyWindowsLifecycleFile(sourcePath, targetPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return errors.New("lifecycle helper source unavailable")
	}
	defer source.Close()

	temporaryPath := targetPath + ".installing"
	_ = os.Remove(temporaryPath)
	temporary, err := os.OpenFile(
		temporaryPath,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0o700,
	)
	if err != nil {
		return errors.New("lifecycle helper creation failed")
	}
	keep := true
	defer func() {
		_ = temporary.Close()
		if keep {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := io.Copy(temporary, source); err != nil {
		return errors.New("lifecycle helper copy failed")
	}
	if err := temporary.Sync(); err != nil {
		return errors.New("lifecycle helper sync failed")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("lifecycle helper close failed")
	}
	if err := secureIdentityFile(temporaryPath); err != nil {
		return err
	}
	if err := replaceFileAtomically(temporaryPath, targetPath); err != nil {
		return errors.New("lifecycle helper publication failed")
	}
	keep = false
	return nil
}

func launchWindowsLifecycleHelper(helperPath, command string) error {
	process := exec.Command(helperPath, command, strconv.Itoa(os.Getpid()))
	process.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	if err := process.Start(); err != nil {
		return errors.New("lifecycle helper start failed")
	}
	return process.Process.Release()
}

func waitForWindowsProcess(processID uint32, timeout time.Duration) error {
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, processID)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return errors.New("service process wait failed")
	}
	defer windows.CloseHandle(process)
	waitMilliseconds := uint32(timeout / time.Millisecond)
	result, err := windows.WaitForSingleObject(process, waitMilliseconds)
	if err != nil {
		return errors.New("service process wait failed")
	}
	if result != windows.WAIT_OBJECT_0 {
		return errors.New("service process exit timed out")
	}
	return nil
}

func runWindowsUpdateHelper() error {
	candidate, err := candidateExecutablePath()
	if err != nil {
		return err
	}
	active, err := installedExecutablePath()
	if err != nil {
		return err
	}
	if err := requireRegularFile(candidate); err != nil {
		_ = startWindowsGatewayService()
		return errors.New("verified Gateway candidate is unavailable")
	}
	if err := replaceFileAtomically(candidate, active); err != nil {
		_ = startWindowsGatewayService()
		return errors.New("Gateway executable replacement failed")
	}
	return startWindowsGatewayService()
}

func startWindowsGatewayService() error {
	manager, err := mgr.Connect()
	if err != nil {
		return errors.New("Service Control Manager unavailable")
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(windowsServiceName)
	if err != nil {
		return errors.New("Gateway service unavailable")
	}
	defer service.Close()
	return ensureWindowsServiceRunning(service)
}

func runWindowsRemovalHelper() (resultErr error) {
	pending, err := gatewayRemovalPending()
	if err != nil {
		return err
	}
	if !pending {
		return errors.New("terminal Gateway removal was not authorized")
	}
	manager, err := mgr.Connect()
	if err != nil {
		return errors.New("Service Control Manager unavailable")
	}
	managerOpen := true
	var service *mgr.Service
	restartOnFailure := false
	defer func() {
		if service != nil {
			service.Close()
		}
		if managerOpen {
			manager.Disconnect()
		}
		if resultErr != nil && restartOnFailure {
			_ = startWindowsGatewayService()
		}
	}()
	service, openErr := manager.OpenService(windowsServiceName)
	if openErr == nil {
		restartOnFailure = true
		if err := stopWindowsService(service); err != nil {
			return err
		}
	} else if !errors.Is(openErr, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return errors.New("Gateway service inspection failed")
	}

	active, err := installedExecutablePath()
	if err != nil {
		return err
	}
	helper, err := os.Executable()
	if err != nil {
		return errors.New("removal helper path unavailable")
	}
	if err := removeWindowsInstalledPackageExcept(active); err != nil {
		return err
	}
	if err := deleteGatewayID(); err != nil {
		return err
	}
	if err := removeWindowsIdentityExceptRemovalState(helper); err != nil {
		return err
	}

	if service != nil {
		if err := service.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return errors.New("Gateway service removal failed")
		}
		service.Close()
		service = nil
	}
	manager.Disconnect()
	managerOpen = false
	restartOnFailure = false

	// Register exact file/directory fallbacks only after the SCM definition is
	// gone. Before that point, any interruption leaves the automatic service,
	// active executable, durable marker and helper able to resume removal.
	scheduleWindowsFinalRemoval(active, helper)

	if err := removeWindowsInstalledPackage(); err != nil {
		return err
	}
	return removeWindowsIdentityState()
}

func removeWindowsInstalledPackageExcept(activeExecutable string) error {
	paths, err := resolveWindowsPackagePaths()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(paths.directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("Gateway installation inspection failed")
	}
	for _, entry := range entries {
		path := filepath.Join(paths.directory, entry.Name())
		if sameWindowsPath(path, activeExecutable) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return errors.New("Gateway installation preparation failed")
		}
	}
	return nil
}

func removeWindowsIdentityExceptRemovalState(helperExecutable string) error {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(paths.directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("Gateway identity inspection failed")
	}
	for _, entry := range entries {
		path := filepath.Join(paths.directory, entry.Name())
		if sameWindowsPath(path, paths.removalPending) ||
			sameWindowsPath(path, paths.workDirectory) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return errors.New("Gateway identity preparation failed")
		}
	}
	workEntries, err := os.ReadDir(paths.workDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("Gateway lifecycle state inspection failed")
	}
	for _, entry := range workEntries {
		path := filepath.Join(paths.workDirectory, entry.Name())
		if sameWindowsPath(path, helperExecutable) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return errors.New("Gateway lifecycle state preparation failed")
		}
	}
	return nil
}

func scheduleWindowsFinalRemoval(activeExecutable, helperExecutable string) {
	packagePaths, packageErr := resolveWindowsPackagePaths()
	identityPaths, identityErr := resolveIdentityPaths()
	if packageErr != nil || identityErr != nil {
		return
	}
	for _, path := range []string{
		activeExecutable,
		packagePaths.directory,
		identityPaths.removalPending,
		helperExecutable,
		identityPaths.workDirectory,
		identityPaths.directory,
	} {
		_ = scheduleWindowsDeletion(path)
	}
}

func removeWindowsInstalledPackage() error {
	paths, err := resolveWindowsPackagePaths()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(paths.directory); err != nil {
		return errors.New("Gateway installation removal failed")
	}
	return nil
}

func removeWindowsIdentityState() error {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	currentExecutable, executableErr := os.Executable()
	entries, readErr := os.ReadDir(paths.directory)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return errors.New("Gateway identity removal failed")
	}
	for _, entry := range entries {
		path := filepath.Join(paths.directory, entry.Name())
		if sameWindowsPath(path, paths.workDirectory) ||
			(executableErr == nil && sameWindowsPath(path, currentExecutable)) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return errors.New("Gateway identity removal failed")
		}
	}
	workEntries, workReadErr := os.ReadDir(paths.workDirectory)
	if workReadErr != nil && !errors.Is(workReadErr, os.ErrNotExist) {
		return errors.New("Gateway lifecycle state removal failed")
	}
	for _, entry := range workEntries {
		path := filepath.Join(paths.workDirectory, entry.Name())
		if executableErr == nil && sameWindowsPath(path, currentExecutable) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return errors.New("Gateway lifecycle state removal failed")
		}
	}
	if executableErr == nil {
		if err := deleteRunningWindowsFile(currentExecutable); err != nil {
			_ = scheduleWindowsDeletion(currentExecutable)
		}
	}
	if err := os.Remove(paths.workDirectory); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = scheduleWindowsDeletion(paths.workDirectory)
	}
	if err := os.Remove(paths.directory); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = scheduleWindowsDeletion(paths.directory)
	}
	return nil
}

func removeWindowsLifecycleHelper() {
	executable, err := os.Executable()
	if err != nil {
		return
	}
	if err := deleteRunningWindowsFile(executable); err != nil {
		_ = scheduleWindowsDeletion(executable)
	}
}

func deleteRunningWindowsFile(path string) error {
	pathPointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(
		pathPointer,
		windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS)
	return windows.SetFileInformationByHandle(
		handle,
		windows.FileDispositionInformationEx,
		(*byte)(unsafe.Pointer(&flags)),
		uint32(unsafe.Sizeof(flags)),
	)
}

func scheduleWindowsDeletion(path string) error {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(pointer, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

func requireRegularFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}
	return nil
}

func sameWindowsPath(first, second string) bool {
	firstAbsolute, firstErr := filepath.Abs(first)
	secondAbsolute, secondErr := filepath.Abs(second)
	return firstErr == nil && secondErr == nil &&
		strings.EqualFold(filepath.Clean(firstAbsolute), filepath.Clean(secondAbsolute))
}
