//go:build windows

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsInternalUpdateCommand   = "internal-update"
	windowsInternalRollbackCommand = "internal-rollback"
	windowsInternalRestartCommand  = "internal-restart"
	windowsInternalRemoveCommand   = "internal-remove"
	windowsCandidateFilename       = "camos-gateway.candidate.exe"
	windowsLifecycleReadyContents  = "camos-gateway-lifecycle-ready-v1\n"
	windowsLifecycleReadyTimeout   = 15 * time.Second
	windowsLifecycleMutexName      = `Global\camOSGatewayLifecycleV1`
	windowsDownloadMutexName       = `Global\camOSGatewayDownloadV1`
	windowsRemovalMutexName        = `Global\camOSGatewayRemovalLifecycleV1`
)

// acquireGatewayLifecycleLock serializes the short destructive/recovery
// boundary across accidental duplicate service processes without creating a
// persistent lock file. An abandoned mutex is valid ownership after a crash.
func acquireGatewayLifecycleLock(timeout time.Duration) (func(), error) {
	return acquireWindowsProcessMutex(windowsLifecycleMutexName, timeout, "lifecycle")
}

func acquireGatewayDownloadLock(timeout time.Duration) (func(), error) {
	return acquireWindowsProcessMutex(windowsDownloadMutexName, timeout, "download")
}

func acquireWindowsProcessMutex(nameValue string, timeout time.Duration, kind string) (func(), error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("Gateway %s lock timeout is invalid", kind)
	}
	// Win32 mutex ownership is thread-affine; keep acquisition and release on
	// the same OS thread even if this Go goroutine blocks during lifecycle work.
	runtime.LockOSThread()
	name, err := windows.UTF16PtrFromString(nameValue)
	if err != nil {
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("Gateway %s lock name is invalid", kind)
	}
	mutex, err := windows.CreateMutex(nil, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if mutex != 0 {
			_ = windows.CloseHandle(mutex)
		}
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("Gateway %s lock creation failed", kind)
	}
	result, err := windows.WaitForSingleObject(mutex, uint32(timeout/time.Millisecond))
	if err != nil || (result != windows.WAIT_OBJECT_0 && result != windows.WAIT_ABANDONED) {
		_ = windows.CloseHandle(mutex)
		runtime.UnlockOSThread()
		return nil, fmt.Errorf("Gateway %s lock timed out", kind)
	}
	return func() {
		_ = windows.ReleaseMutex(mutex)
		_ = windows.CloseHandle(mutex)
		runtime.UnlockOSThread()
	}, nil
}

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

func applyGatewayUpdate(candidatePath string) (gatewayReplacementOutcome, error) {
	expectedCandidate, err := candidateExecutablePath()
	if err != nil {
		return gatewayReplacementPreCommit, err
	}
	if !sameWindowsPath(candidatePath, expectedCandidate) {
		return gatewayReplacementPreCommit, errors.New("Gateway candidate path is invalid")
	}
	if err := requireRegularFile(candidatePath); err != nil {
		return gatewayReplacementPreCommit, fmt.Errorf("Gateway candidate is invalid: %w", err)
	}
	helper, err := prepareWindowsLifecycleHelper("update")
	if err != nil {
		return gatewayReplacementPreCommit, err
	}
	if err := launchWindowsLifecycleHelper(helper, windowsInternalUpdateCommand); err != nil {
		return gatewayReplacementPreCommit, err
	}
	return gatewayReplacementHelperHandoff, nil
}

func applyGatewayRollback(previousPath string) (gatewayReplacementOutcome, error) {
	expectedPrevious, err := previousExecutablePath()
	if err != nil {
		return gatewayReplacementPreCommit, err
	}
	if !sameWindowsPath(previousPath, expectedPrevious) {
		return gatewayReplacementPreCommit, errors.New("Gateway rollback path is invalid")
	}
	if err := requireRegularFile(previousPath); err != nil {
		return gatewayReplacementPreCommit, errors.New("Gateway rollback executable is unavailable")
	}
	// Build rollback authority from the already validated last-known-good image,
	// not from an active pathname that may be the reason recovery is running.
	helper, err := prepareWindowsLifecycleHelperFrom(previousPath, "rollback")
	if err != nil {
		return gatewayReplacementPreCommit, err
	}
	if err := launchWindowsLifecycleHelper(helper, windowsInternalRollbackCommand); err != nil {
		return gatewayReplacementPreCommit, err
	}
	return gatewayReplacementHelperHandoff, nil
}

func beginGatewayRestart() error {
	helper, err := prepareWindowsLifecycleHelper("restart")
	if err != nil {
		return err
	}
	return launchWindowsLifecycleHelper(helper, windowsInternalRestartCommand)
}

func beginGatewayRemoval() error {
	helper, err := prepareWindowsLifecycleHelper("remove")
	if err != nil {
		return err
	}
	return launchWindowsLifecycleHelper(helper, windowsInternalRemoveCommand)
}

func platformRemovalStatePresent() (bool, error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return false, err
	}
	return windowsRemovalHelperStatePresent(paths.workDirectory)
}

func windowsRemovalHelperStatePresent(workDirectory string) (bool, error) {
	entries, err := os.ReadDir(workDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("Windows terminal removal state is unavailable")
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "camos-gateway-remove-") &&
			(strings.HasSuffix(name, ".exe") || strings.HasSuffix(name, ".exe.installing")) {
			return true, nil
		}
	}
	return false, nil
}

func handleInternalPlatformCommand(arguments []string) (bool, error) {
	if len(arguments) == 0 {
		return false, nil
	}
	command := arguments[0]
	if command != windowsInternalUpdateCommand &&
		command != windowsInternalRollbackCommand &&
		command != windowsInternalRestartCommand &&
		command != windowsInternalRemoveCommand {
		return false, nil
	}
	if len(arguments) != 3 {
		return true, errors.New("invalid internal lifecycle invocation")
	}
	parent, err := strconv.ParseUint(arguments[1], 10, 32)
	if err != nil || parent == 0 {
		return true, errors.New("invalid internal lifecycle invocation")
	}
	readyPath, err := validateWindowsLifecycleReadyPath(arguments[2], uint32(parent))
	if err != nil {
		return true, err
	}
	if err := validateWindowsLifecyclePreparation(command); err != nil {
		return true, err
	}
	if err := signalWindowsLifecycleReady(readyPath); err != nil {
		return true, err
	}
	if err := waitForWindowsProcess(uint32(parent), serviceTransitionTimeout); err != nil {
		return true, err
	}
	return true, runWindowsLifecycleCommandAfterParent(command)
}

func runWindowsLifecycleCommandAfterParent(command string) (resultErr error) {
	originalCommand := command
	retryRemovalOnFailure := command == windowsInternalRemoveCommand
	// A removal parent reports an orderly service stop after the child has
	// acknowledged readiness. From this point onward the child must restore a
	// same-boot actor on every failure, including authority and lock failures
	// that occur before runWindowsRemovalHelper is entered.
	defer func() {
		if resultErr != nil && retryRemovalOnFailure {
			restoreWindowsRemovalRetryAuthority()
		}
	}()
	// The parent deliberately releases its locks before exiting. Reacquire them
	// only after that exit so the transient helper serializes its actual byte or
	// removal mutation without deadlocking the readiness handshake. A committed
	// removal does not wait behind a slow download lock.
	removalPending, err := windowsRemovalAuthorized()
	if err != nil {
		return err
	}
	if removalPending {
		command = windowsInternalRemoveCommand
		retryRemovalOnFailure = true
	}
	var releaseLifecycle func()
	if command == windowsInternalUpdateCommand || command == windowsInternalRollbackCommand {
		releaseLifecycle, err = acquireGatewayUpdateLocks(lifecycleOperationLockWait)
	} else {
		releaseLifecycle, err = acquireGatewayLifecycleLock(lifecycleOperationLockWait)
	}
	if err != nil {
		return err
	}
	defer releaseLifecycle()
	if command != windowsInternalRemoveCommand {
		pending, pendingErr := windowsRemovalAuthorized()
		if pendingErr != nil {
			return pendingErr
		}
		command = windowsLifecycleCommandWithRemovalPriority(command, pending)
		if command == windowsInternalRemoveCommand {
			retryRemovalOnFailure = true
		}
	}

	switch command {
	case windowsInternalUpdateCommand:
		err = runWindowsUpdateHelper()
	case windowsInternalRollbackCommand:
		err = runWindowsRollbackHelper()
	case windowsInternalRestartCommand:
		err = startWindowsGatewayService()
	case windowsInternalRemoveCommand:
		if windowsRemovalHelperNeedsNormalization(originalCommand, command) {
			err = handoffToWindowsRemovalHelper()
		} else {
			err = runWindowsRemovalHelper()
		}
	}
	if command != windowsInternalRemoveCommand {
		removeWindowsLifecycleHelper()
	}
	return err
}

func windowsLifecycleCommandWithRemovalPriority(command string, removalPending bool) string {
	if removalPending {
		return windowsInternalRemoveCommand
	}
	return command
}

func windowsRemovalHelperNeedsNormalization(originalCommand, finalCommand string) bool {
	return finalCommand == windowsInternalRemoveCommand &&
		originalCommand != windowsInternalRemoveCommand
}

func handoffToWindowsRemovalHelper() error {
	helper, err := prepareWindowsLifecycleHelper("remove")
	if err != nil {
		return err
	}
	return launchWindowsLifecycleHelper(helper, windowsInternalRemoveCommand)
}

func prepareWindowsLifecycleHelper(kind string) (string, error) {
	source, err := os.Executable()
	if err != nil {
		return "", errors.New("Gateway executable path unavailable")
	}
	return prepareWindowsLifecycleHelperFrom(source, kind)
}

func prepareWindowsLifecycleHelperFrom(source, kind string) (string, error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return "", err
	}
	if err := prepareIdentityWorkDirectory(paths); err != nil {
		return "", err
	}
	if err := cleanupStaleWindowsLifecycleFiles(paths.workDirectory); err != nil {
		return "", err
	}
	helper, err := newWindowsLifecycleHelperPath(paths.workDirectory, kind)
	if err != nil {
		return "", err
	}
	if err := copyWindowsLifecycleFile(source, helper); err != nil {
		return "", err
	}
	return helper, nil
}

func newWindowsLifecycleHelperPath(workDirectory, kind string) (string, error) {
	switch kind {
	case "update", "rollback", "restart", "remove":
	default:
		return "", errors.New("lifecycle helper kind is invalid")
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", errors.New("lifecycle helper name generation failed")
	}
	// A random 128-bit per-attempt name must never intentionally be reused.
	// MoveFileEx delayed-delete registrations are pathname-based and can survive
	// after an old helper disappears; PID-based names could therefore cause a
	// later valid helper to be deleted at reboot.
	return filepath.Join(
		workDirectory,
		fmt.Sprintf("camos-gateway-%s-%s.exe", kind, hex.EncodeToString(nonce[:])),
	), nil
}

func copyWindowsLifecycleFile(sourcePath, targetPath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return errors.New("lifecycle helper source unavailable")
	}
	defer source.Close()
	sourceInfo, err := source.Stat()
	if err != nil || !sourceInfo.Mode().IsRegular() || sourceInfo.Size() <= 0 {
		return errors.New("lifecycle helper source is invalid")
	}

	temporaryPath := targetPath + ".installing"
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
	written, err := io.Copy(temporary, source)
	if err != nil || written != sourceInfo.Size() {
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
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	readyPath := filepath.Join(
		paths.workDirectory,
		fmt.Sprintf(".lifecycle-ready-%d", os.Getpid()),
	)
	_ = os.Remove(readyPath)
	process := exec.Command(
		helperPath,
		command,
		strconv.Itoa(os.Getpid()),
		readyPath,
	)
	process.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_BREAKAWAY_FROM_JOB | windows.CREATE_NO_WINDOW,
		HideWindow:    true,
	}
	if err := process.Start(); err != nil {
		return errors.New("lifecycle helper start failed")
	}
	ready := false
	deadline := time.Now().Add(windowsLifecycleReadyTimeout)
	for time.Now().Before(deadline) {
		contents, readErr := os.ReadFile(readyPath)
		if readErr == nil && string(contents) == windowsLifecycleReadyContents {
			ready = true
			break
		}
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = os.Remove(readyPath)
	if !ready {
		_ = process.Process.Kill()
		_, _ = process.Process.Wait()
		return errors.New("lifecycle helper readiness timed out")
	}
	return process.Process.Release()
}

func validateWindowsLifecycleReadyPath(path string, parentID uint32) (string, error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return "", err
	}
	expected := filepath.Join(
		paths.workDirectory,
		fmt.Sprintf(".lifecycle-ready-%d", parentID),
	)
	if !sameWindowsPath(path, expected) {
		return "", errors.New("lifecycle helper readiness path is invalid")
	}
	return expected, nil
}

func signalWindowsLifecycleReady(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("lifecycle helper readiness publication failed")
	}
	keep := true
	defer func() {
		_ = file.Close()
		if keep {
			_ = os.Remove(path)
		}
	}()
	if _, err := io.WriteString(file, windowsLifecycleReadyContents); err != nil {
		return errors.New("lifecycle helper readiness publication failed")
	}
	if err := file.Sync(); err != nil {
		return errors.New("lifecycle helper readiness sync failed")
	}
	if err := file.Close(); err != nil {
		return errors.New("lifecycle helper readiness close failed")
	}
	if err := secureIdentityFile(path); err != nil {
		return err
	}
	keep = false
	return nil
}

func validateWindowsLifecyclePreparation(command string) error {
	manager, err := mgr.Connect()
	if err != nil {
		return errors.New("Service Control Manager unavailable")
	}
	manager.Disconnect()
	pending, err := windowsRemovalAuthorized()
	if err != nil {
		return err
	}
	if pending {
		// The command is converted to removal again after the parent exits.
		// Do not let stale update/restart prerequisites obstruct committed
		// removal during this readiness window.
		return nil
	}
	switch command {
	case windowsInternalUpdateCommand:
		return validateWindowsPendingExecutable(false)
	case windowsInternalRollbackCommand:
		return validateWindowsPendingExecutable(true)
	case windowsInternalRemoveCommand:
		if !pending {
			return errors.New("terminal Gateway removal was not authorized")
		}
	}
	return nil
}

func validateWindowsPendingExecutable(previous bool) error {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	record, err := loadUpdatePending(paths)
	if err != nil || record == nil {
		return errors.New("pending Gateway update is unavailable")
	}
	path, version := "", record.toVersion
	if previous {
		path, err = previousExecutablePath()
		version = record.fromVersion
	} else {
		path, err = candidateExecutablePath()
	}
	if err != nil {
		return err
	}
	hash, _, err := hashLifecycleFile(path)
	if err != nil {
		return errors.New("pending Gateway executable is unavailable")
	}
	expectedHash := record.candidateSHA256
	if previous {
		expectedHash = record.activeSHA256
	}
	if hash != expectedHash {
		return errors.New("pending Gateway executable hash changed")
	}
	if _, err := inspectGatewayCandidate(path, version, record.target); err != nil {
		return errors.New("pending Gateway executable identity is invalid")
	}
	return nil
}

func cleanupStaleWindowsLifecycleFiles(workDirectory string) error {
	entries, err := os.ReadDir(workDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("Gateway lifecycle state inspection failed")
	}
	currentExecutable, currentErr := os.Executable()
	for _, entry := range entries {
		path := filepath.Join(workDirectory, entry.Name())
		if !windowsLifecycleFileIsStale(path, currentExecutable, currentErr == nil) {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			if scheduleErr := scheduleWindowsDeletion(path); scheduleErr != nil {
				return errors.New("stale lifecycle helper cleanup failed")
			}
		}
	}
	return nil
}

func windowsLifecycleFileIsStale(path, currentExecutable string, currentKnown bool) bool {
	name := filepath.Base(path)
	lifecycleFile := strings.HasPrefix(name, "camos-gateway-") &&
		(strings.HasSuffix(name, ".exe") || strings.HasSuffix(name, ".exe.installing"))
	readyFile := strings.HasPrefix(name, ".lifecycle-ready-")
	if !lifecycleFile && !readyFile {
		return false
	}
	if readyFile {
		return true
	}
	// A removal helper may itself be the executable configured in SCM for the
	// post-reboot finalization pass. Keep that current actor until its successor
	// has acknowledged readiness and durably redirected SCM.
	return currentKnown && !sameWindowsPath(path, currentExecutable)
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
	if err := validateWindowsPendingExecutable(false); err != nil {
		_ = startWindowsGatewayService()
		return err
	}
	if err := validateWindowsPendingExecutable(true); err != nil {
		_ = startWindowsGatewayService()
		return errors.New("last-known-good Gateway executable is unavailable")
	}
	candidate, err := candidateExecutablePath()
	if err != nil {
		return err
	}
	active, err := installedExecutablePath()
	if err != nil {
		return err
	}
	if err := replaceFileAtomically(candidate, active); err != nil {
		_ = startWindowsGatewayService()
		return errors.New("Gateway executable replacement failed")
	}
	if err := startWindowsGatewayService(); err != nil {
		_ = writeLifecycleStatus(lifecycleStatusRestart, BuildVersion, err.Error())
		previous, previousErr := previousExecutablePath()
		if previousErr != nil {
			return errors.New("Gateway start failed and rollback path is unavailable")
		}
		if validateErr := validateWindowsPendingExecutable(true); validateErr != nil {
			return errors.New("Gateway start failed and last-known-good executable is invalid")
		}
		if replaceErr := replaceFileAtomically(previous, active); replaceErr != nil {
			return errors.New("Gateway start failed and rollback replacement failed")
		}
		if restartErr := startWindowsGatewayService(); restartErr != nil {
			return errors.New("Gateway rollback committed but service restart failed")
		}
		return errors.New("Gateway candidate failed to start and was rolled back")
	}
	return nil
}

func runWindowsRollbackHelper() error {
	if err := validateWindowsPendingExecutable(true); err != nil {
		_ = startWindowsGatewayService()
		return err
	}
	previous, err := previousExecutablePath()
	if err != nil {
		_ = startWindowsGatewayService()
		return err
	}
	active, err := installedExecutablePath()
	if err != nil {
		_ = startWindowsGatewayService()
		return err
	}
	if err := replaceFileAtomically(previous, active); err != nil {
		_ = startWindowsGatewayService()
		return errors.New("Gateway rollback replacement failed")
	}
	if err := startWindowsGatewayService(); err != nil {
		_ = writeLifecycleStatus(lifecycleStatusRollback, BuildVersion, err.Error())
		return errors.New("Gateway rollback committed but service restart failed")
	}
	return nil
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

func triggerWindowsGatewayServiceWithRetry() error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := triggerWindowsGatewayService(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt < 2 {
			time.Sleep(serviceStatePollInterval)
		}
	}
	return lastErr
}

type windowsServiceTriggerAction uint8

const (
	windowsServiceTriggerReady windowsServiceTriggerAction = iota
	windowsServiceTriggerStart
	windowsServiceTriggerContinue
	windowsServiceTriggerWait
)

func windowsServiceTriggerActionForState(state svc.State) (windowsServiceTriggerAction, error) {
	switch state {
	case svc.Running, svc.StartPending, svc.ContinuePending:
		return windowsServiceTriggerReady, nil
	case svc.Stopped:
		return windowsServiceTriggerStart, nil
	case svc.Paused:
		return windowsServiceTriggerContinue, nil
	case svc.StopPending, svc.PausePending:
		return windowsServiceTriggerWait, nil
	default:
		return windowsServiceTriggerReady, fmt.Errorf("Gateway service has invalid state %d", state)
	}
}

func triggerWindowsGatewayService() error {
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
	deadline := time.Now().Add(serviceTransitionTimeout)
	for {
		status, err := service.Query()
		if err != nil {
			return errors.New("Gateway service state query failed")
		}
		action, err := windowsServiceTriggerActionForState(status.State)
		if err != nil {
			return err
		}
		switch action {
		case windowsServiceTriggerReady:
			return nil
		case windowsServiceTriggerStart:
			if err := service.Start(); err != nil &&
				!errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
				return errors.New("Gateway service start failed")
			}
			return nil
		case windowsServiceTriggerContinue:
			if _, err := service.Control(svc.Continue); err != nil {
				return errors.New("Gateway service continue failed")
			}
			return nil
		case windowsServiceTriggerWait:
			if time.Now().After(deadline) {
				return errors.New("Gateway service transition timed out")
			}
			time.Sleep(serviceStatePollInterval)
		}
	}
}

func restoreWindowsRemovalRetryAuthority() {
	if triggerWindowsGatewayServiceWithRetry() == nil {
		return
	}
	// A service that is absent or marked for deletion cannot accept a restart.
	// Keep the retry actor transient by handing off to another acknowledged
	// removal helper rather than creating a second permanent service.
	for attempt := 0; attempt < 3; attempt++ {
		if handoffToWindowsRemovalHelper() == nil {
			return
		}
		if attempt < 2 {
			time.Sleep(serviceStatePollInterval)
		}
	}
}

func runWindowsRemovalHelper() (resultErr error) {
	pending, err := windowsRemovalAuthorized()
	if err != nil {
		return err
	}
	if !pending {
		return errors.New("terminal Gateway removal was not authorized")
	}
	removalMutex, err := acquireWindowsRemovalMutex(serviceTransitionTimeout)
	if err != nil {
		return err
	}
	defer releaseWindowsRemovalMutex(removalMutex)
	manager, err := mgr.Connect()
	if err != nil {
		return errors.New("Service Control Manager unavailable")
	}
	managerOpen := true
	var service *mgr.Service
	defer func() {
		if service != nil {
			service.Close()
		}
		if managerOpen {
			manager.Disconnect()
		}
	}()
	helper, err := os.Executable()
	if err != nil {
		return errors.New("removal helper path unavailable")
	}
	service, err = openOrCreateWindowsRemovalService(manager, helper)
	if err != nil {
		return err
	}
	if service != nil {
		if err := stopWindowsService(service); err != nil {
			return err
		}
	}

	active, err := installedExecutablePath()
	if err != nil {
		return err
	}
	if service != nil {
		// Before registering any pathname for reboot deletion, redirect the
		// existing service to this transient helper. If a later registration
		// fails or the machine reboots mid-attempt, SCM can still execute code
		// that sees removal.pending and continues removal-only work.
		if err := configureWindowsRemovalRetryService(service, helper); err != nil {
			return err
		}
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
	packagePending, err := windowsPackageRemovalPending()
	if err != nil {
		return err
	}
	if packagePending {
		// Stage one always leaves the redirected automatic service, strict marker,
		// and helper intact. Reboot consumes the active/package registrations;
		// only then can the same service enter finalization below. Resetting the
		// recovery action prevents same-boot retry churn while preserving boot
		// start as native retry authority.
		if err := scheduleWindowsPackageRemoval(active); err != nil {
			return err
		}
		if service == nil {
			// With an already-absent service, the current helper is the final native
			// actor. Queue the remaining marker/helper state for the same reboot.
			return scheduleWindowsRemovalState(helper)
		}
		if err := armWindowsRemovalForReboot(service); err != nil {
			return err
		}
		service.Close()
		service = nil
		manager.Disconnect()
		managerOpen = false
		return nil
	}
	// With the package and GatewayID gone, durably register every remaining
	// protected removal pathname before deleting the last SCM actor. The marker
	// and unique helper deliberately stay visible as commissioning guards until
	// reboot consumes the queue. Any registration failure leaves the marker and
	// service untouched so local/native retry authority is retained.
	if err := scheduleWindowsRemovalState(helper); err != nil {
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
	return nil
}

func acquireWindowsRemovalMutex(timeout time.Duration) (windows.Handle, error) {
	runtime.LockOSThread()
	name, err := windows.UTF16PtrFromString(windowsRemovalMutexName)
	if err != nil {
		runtime.UnlockOSThread()
		return 0, errors.New("Gateway removal mutex name is invalid")
	}
	mutex, err := windows.CreateMutex(nil, false, name)
	if err != nil && !errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		if mutex != 0 {
			_ = windows.CloseHandle(mutex)
		}
		runtime.UnlockOSThread()
		return 0, errors.New("Gateway removal mutex creation failed")
	}
	result, err := windows.WaitForSingleObject(mutex, uint32(timeout/time.Millisecond))
	if err != nil || (result != windows.WAIT_OBJECT_0 && result != windows.WAIT_ABANDONED) {
		_ = windows.CloseHandle(mutex)
		runtime.UnlockOSThread()
		return 0, errors.New("Gateway removal mutex wait failed")
	}
	return mutex, nil
}

func releaseWindowsRemovalMutex(mutex windows.Handle) {
	if mutex == 0 {
		return
	}
	_ = windows.ReleaseMutex(mutex)
	_ = windows.CloseHandle(mutex)
	runtime.UnlockOSThread()
}

func openOrCreateWindowsRemovalService(
	manager *mgr.Mgr,
	helperExecutable string,
) (*mgr.Service, error) {
	service, err := manager.OpenService(windowsServiceName)
	if err == nil {
		return service, nil
	}
	if errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		// A marked service cannot be recreated until every outstanding handle is
		// closed. The caller retains the strict marker and uses checked reboot
		// deletion registrations as its already-absent-service fallback.
		return nil, nil
	}
	if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil, errors.New("Gateway service inspection failed")
	}
	config := windowsServiceConfig(helperExecutable)
	service, err = manager.CreateService(
		windowsServiceName,
		helperExecutable,
		config,
		"service",
	)
	if err != nil {
		return nil, errors.New("Gateway removal retry service creation failed")
	}
	actual, err := service.Config()
	if err != nil || !windowsServiceConfigurationMatches(actual, config) {
		service.Close()
		// Preserve the newly created automatic service as reboot retry authority.
		// Its executable and arguments were supplied directly to CreateService;
		// deleting it because the verification read failed would strand a valid
		// removal marker with no native actor.
		return nil, errors.New("Gateway removal retry service verification failed")
	}
	return service, nil
}

func windowsRemovalAuthorized() (bool, error) {
	pending, err := gatewayRemovalPending()
	if err != nil || pending {
		return pending, err
	}
	nativePending, err := platformRemovalStatePresent()
	if err != nil || !nativePending {
		return false, err
	}
	packagePending, err := windowsPackageRemovalPending()
	if err != nil || packagePending {
		return false, err
	}
	identityPresent, err := gatewayIDCommitPresent()
	if err != nil {
		return false, errors.New("terminal removal GatewayID state is unavailable")
	}
	if identityPresent {
		return false, errors.New("terminal removal helper conflicts with a Gateway identity")
	}
	return windowsNativeRemovalContinuationAllowed(nativePending, packagePending, identityPresent), nil
}

func windowsNativeRemovalContinuationAllowed(
	nativePending, packagePending, identityPresent bool,
) bool {
	return nativePending && !packagePending && !identityPresent
}

func configureWindowsRemovalRetryService(service *mgr.Service, helperExecutable string) error {
	expected := windowsServiceConfig(helperExecutable)
	if err := service.UpdateConfig(expected); err != nil {
		return errors.New("Gateway removal retry service configuration failed")
	}
	configured, err := service.Config()
	if err != nil {
		return errors.New("Gateway removal retry service verification failed")
	}
	if len(configured.Dependencies) != 0 {
		if err := clearWindowsServiceDependencies(service); err != nil {
			return errors.New("Gateway removal retry service dependency cleanup failed")
		}
	}
	actual, err := service.Config()
	if err != nil || !windowsServiceConfigurationMatches(actual, expected) {
		return errors.New("Gateway removal retry service verification failed")
	}
	return nil
}

func armWindowsRemovalForReboot(service *mgr.Service) error {
	if err := service.ResetRecoveryActions(); err != nil {
		return errors.New("Gateway removal retry policy reset failed")
	}
	if err := service.SetRecoveryActionsOnNonCrashFailures(false); err != nil {
		return errors.New("Gateway removal retry policy reset failed")
	}
	actions, actionsErr := service.RecoveryActions()
	nonCrash, flagErr := service.RecoveryActionsOnNonCrashFailures()
	if actionsErr != nil || flagErr != nil || len(actions) != 0 || nonCrash {
		return errors.New("Gateway removal retry policy verification failed")
	}
	return nil
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

func scheduleWindowsPackageRemoval(activeExecutable string) error {
	packagePaths, packageErr := resolveWindowsPackagePaths()
	if packageErr != nil {
		return errors.New("final Gateway removal paths are unavailable")
	}
	for _, path := range []string{
		activeExecutable,
		packagePaths.directory,
	} {
		if err := scheduleWindowsDeletionWithRetry(path); err != nil {
			return fmt.Errorf("Gateway package deletion scheduling failed: %w", err)
		}
	}
	return nil
}

func windowsPackageRemovalPending() (bool, error) {
	packagePaths, err := resolveWindowsPackagePaths()
	if err != nil {
		return false, errors.New("Gateway package removal path is unavailable")
	}
	info, err := os.Lstat(packagePaths.directory)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("Gateway package removal state is unavailable")
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, errors.New("Gateway package removal state is invalid")
	}
	return true, nil
}

func scheduleWindowsRemovalState(helperExecutable string) error {
	identityPaths, err := resolveIdentityPaths()
	if err != nil {
		return errors.New("final Gateway removal paths are unavailable")
	}
	// The marker is registered first so every successful prefix of this checked
	// sequence is fail-closed: if registration later fails and reboot intervenes,
	// either the strict marker remains or its removal exposes only the protected
	// helper tombstone with package and GatewayID already absent. The unique
	// helper then precedes its parent directories.
	for _, path := range []string{
		identityPaths.removalPending,
		helperExecutable,
		identityPaths.workDirectory,
		identityPaths.directory,
	} {
		if err := scheduleWindowsDeletionWithRetry(path); err != nil {
			return fmt.Errorf("Gateway removal-state deletion scheduling failed: %w", err)
		}
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
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(pointer, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

func scheduleWindowsDeletionWithRetry(path string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := scheduleWindowsDeletion(path); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt < 2 {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return lastErr
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
