//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func quotePOSIXShellArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// acquireGatewayLifecycleLock serializes the short destructive/recovery
// boundary across accidental duplicate service processes. Lock the stable
// parent of the identity directory rather than adding a persistent state file.
// This also lets explicit commissioning take the lock before it creates the
// canonical identity directory, so it cannot recreate that name while a
// terminal-removal helper is retiring the prior generation. flock is released
// by the kernel if a process exits or crashes.
func acquireGatewayLifecycleLock(timeout time.Duration) (func(), error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return nil, err
	}
	lockPath, err := posixLifecycleLockPath(paths)
	if err != nil {
		return nil, err
	}
	return acquirePOSIXPathLock(lockPath, timeout, "lifecycle")
}

func posixLifecycleLockPath(paths identityPaths) (string, error) {
	directory := filepath.Clean(paths.directory)
	parent := filepath.Dir(directory)
	if !filepath.IsAbs(directory) || directory == string(os.PathSeparator) ||
		parent == directory {
		return "", errors.New("Gateway lifecycle lock path is invalid")
	}
	return parent, nil
}

// Downloads use the immutable GatewayID inode as a separate advisory lock so
// a slow body transfer never delays terminal-removal authority on the lifecycle
// lock. Neither lock creates persistent state.
func acquireGatewayDownloadLock(timeout time.Duration) (func(), error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return nil, err
	}
	return acquirePOSIXPathLock(paths.gatewayID, timeout, "download")
}

// The committed marker is also the transient POSIX removal-setup lock. It
// serializes duplicate helper publishers with the validating helper without
// adding another persistent lock file.
func acquireGatewayRemovalLock(timeout time.Duration) (func(), error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return nil, err
	}
	return acquirePOSIXPathLock(paths.removalPending, timeout, "removal")
}

func acquirePOSIXPathLock(path string, timeout time.Duration, kind string) (func(), error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("Gateway %s lock timeout is invalid", kind)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("Gateway %s lock is unavailable", kind)
	}
	deadline := time.Now().Add(timeout)
	for {
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			// A waiter may have opened the old identity-directory or GatewayID
			// inode before terminal removal unlinked it. Revalidate only after
			// flock succeeds so that stale waiters cannot become authority on an
			// inode which is no longer reachable through the canonical name.
			if err := validatePOSIXLockPath(file, path); err != nil {
				_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
				_ = file.Close()
				return nil, fmt.Errorf("Gateway %s lock is stale", kind)
			}
			return func() {
				_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("Gateway %s lock failed", kind)
		}
		if !time.Now().Before(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("Gateway %s lock timed out", kind)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func validatePOSIXLockPath(file *os.File, path string) error {
	if file == nil {
		return errors.New("open lock path is unavailable")
	}
	opened, err := file.Stat()
	if err != nil {
		return err
	}
	named, err := os.Lstat(path)
	if err != nil || named.Mode()&os.ModeSymlink != 0 || !os.SameFile(opened, named) {
		return errors.New("open lock path is no longer current")
	}
	return nil
}

func identityRemovalTombstonePath(paths identityPaths) (string, error) {
	directory := filepath.Clean(paths.directory)
	if directory == "." || directory == string(os.PathSeparator) {
		return "", errors.New("terminal removal identity path is invalid")
	}
	tombstone := filepath.Clean(directory + ".removing")
	if tombstone == directory || filepath.Dir(tombstone) != filepath.Dir(directory) {
		return "", errors.New("terminal removal tombstone path is invalid")
	}
	return tombstone, nil
}

func posixRemovalTombstonePresent(paths identityPaths) (bool, error) {
	tombstone, err := identityRemovalTombstonePath(paths)
	if err != nil {
		return false, err
	}
	_, err = os.Lstat(tombstone)
	if err == nil {
		// Any object at this root-owned reserved path blocks normal lifecycle
		// work. Only the native finalizer may interpret and remove a directory.
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, errors.New("terminal removal tombstone state is unavailable")
}

// posixRemovalFinalizerPrefix is executed by the already-published native
// unit/job. The Go helper must atomically rename the canonical identity root to
// the fixed tombstone while holding both lifecycle flocks. Only that rename
// lets the shell delete recursively. If the machine reboots after the rename,
// the native actor can finish the tombstone without executing vanished bytes.
func posixRemovalFinalizerPrefix(paths identityPaths, helperPath string) (string, error) {
	tombstone, err := identityRemovalTombstonePath(paths)
	if err != nil {
		return "", err
	}
	quote := quotePOSIXShellArgument
	return fmt.Sprintf(
		"set -e; if [ -e %s ] || [ -L %s ]; then test -d %s; test ! -L %s; test -f %s; test ! -L %s; test -f %s; test ! -L %s; test -x %s; %s internal-remove; fi; test ! -e %s; test ! -L %s; if [ -e %s ] || [ -L %s ]; then test -d %s; test ! -L %s; /bin/rm -rf -- %s; fi",
		quote(paths.directory),
		quote(paths.directory),
		quote(paths.directory),
		quote(paths.directory),
		quote(paths.removalPending),
		quote(paths.removalPending),
		quote(helperPath),
		quote(helperPath),
		quote(helperPath),
		quote(helperPath),
		quote(paths.directory),
		quote(paths.directory),
		quote(tombstone),
		quote(tombstone),
		quote(tombstone),
		quote(tombstone),
		quote(tombstone),
	), nil
}

// posixRemovalHelperPreparationRequired keeps an old process from recreating
// the helper tree after the native finalizer has crossed the GatewayID deletion
// boundary. A retained marker with no native actor is instead a legitimate
// interrupted setup and must still be repaired.
func posixRemovalHelperPreparationRequired(paths identityPaths) (bool, error) {
	_, markerPresent, err := readRemovalMarker(paths.removalPending)
	if err != nil {
		return false, err
	}
	identityPresent, err := gatewayIDCommitPresent()
	if err != nil {
		return false, err
	}
	return decidePOSIXRemovalHelperPreparation(
		markerPresent,
		identityPresent,
		func() (bool, error) { return platformRemovalStatePresent() },
	)
}

func decidePOSIXRemovalHelperPreparation(
	markerPresent bool,
	identityPresent bool,
	readNativeState func() (bool, error),
) (bool, error) {
	if !markerPresent {
		if identityPresent {
			return false, errors.New("GatewayID exists without terminal removal authority")
		}
		// Both canonical identity and marker are absent. This is the terminal
		// state, including a duplicate process that outlived finalization.
		return false, nil
	}
	if identityPresent {
		return true, nil
	}
	if readNativeState == nil {
		return false, errors.New("terminal removal native state is unavailable")
	}
	nativePending, err := readNativeState()
	if err != nil {
		return false, err
	}
	if nativePending {
		// The validating helper already removed GatewayID. Do not let an old
		// controller reinstall the helper while the shell wrapper owns final
		// identity/native-state cleanup.
		return false, nil
	}
	return true, nil
}

func installedExecutablePath() (string, error) {
	return gatewayExecutablePath, nil
}

func installedFFmpegPath() (string, error) {
	return gatewayFFmpegPath, nil
}

func candidateExecutablePath() (string, error) {
	return gatewayExecutablePath + ".candidate", nil
}

func recoverPOSIXCandidateStartupFailure(gatewayID uuid.UUID, failure error) error {
	_, recoveryErr := recoverCandidateAfterStartupFailure(gatewayID, failure)
	if errors.Is(recoveryErr, errTerminalRemovalCommitted) {
		return beginGatewayRemoval()
	}
	return recoveryErr
}

// applyGatewayUpdate publishes only the fully downloaded, already verified
// candidate. POSIX keeps executing the previous inode until the controller
// exits, at which point launchd/systemd restarts the complete new executable.
func applyGatewayUpdate(candidatePath string) (gatewayReplacementOutcome, error) {
	expectedCandidate, err := candidateExecutablePath()
	if err != nil {
		return gatewayReplacementPreCommit, err
	}
	if filepath.Clean(candidatePath) != filepath.Clean(expectedCandidate) {
		return gatewayReplacementPreCommit, errors.New("Gateway update candidate path is invalid")
	}
	info, err := os.Lstat(candidatePath)
	if err != nil {
		return gatewayReplacementPreCommit, fmt.Errorf("Gateway update candidate unavailable: %w", err)
	}
	if !info.Mode().IsRegular() {
		return gatewayReplacementPreCommit, errors.New("Gateway update candidate is not a regular file")
	}
	if err := secureInstalledExecutable(candidatePath); err != nil {
		return gatewayReplacementPreCommit, fmt.Errorf("Gateway update candidate permissions failed: %w", err)
	}
	if err := syncRegularFile(candidatePath); err != nil {
		return gatewayReplacementPreCommit, fmt.Errorf("Gateway update candidate sync failed: %w", err)
	}
	if err := replaceFileAtomically(candidatePath, gatewayExecutablePath); err != nil {
		return gatewayReplacementPreCommit, fmt.Errorf("Gateway executable replacement failed: %w", err)
	}
	if err := syncParentDirectory(gatewayExecutablePath); err != nil {
		return gatewayReplacementPostCommit, fmt.Errorf("Gateway executable directory sync failed after commit: %w", err)
	}
	return gatewayReplacementPostCommit, nil
}

func applyGatewayRollback(previousPath string) (gatewayReplacementOutcome, error) {
	expectedPrevious, err := previousExecutablePath()
	if err != nil {
		return gatewayReplacementPreCommit, err
	}
	if filepath.Clean(previousPath) != filepath.Clean(expectedPrevious) {
		return gatewayReplacementPreCommit, errors.New("Gateway rollback path is invalid")
	}
	if err := secureInstalledExecutable(previousPath); err != nil {
		return gatewayReplacementPreCommit, fmt.Errorf("Gateway rollback executable permissions failed: %w", err)
	}
	if err := syncRegularFile(previousPath); err != nil {
		return gatewayReplacementPreCommit, fmt.Errorf("Gateway rollback executable sync failed: %w", err)
	}
	if err := replaceFileAtomically(previousPath, gatewayExecutablePath); err != nil {
		return gatewayReplacementPreCommit, fmt.Errorf("Gateway rollback replacement failed: %w", err)
	}
	if err := syncParentDirectory(gatewayExecutablePath); err != nil {
		return gatewayReplacementPostCommit, fmt.Errorf("Gateway rollback directory sync failed after commit: %w", err)
	}
	return gatewayReplacementPostCommit, nil
}

// A normal clean exit is sufficient: both launchd and systemd supervise the
// service and restart it. No persistent restart helper is necessary on POSIX.
func beginGatewayRestart() error {
	return nil
}

func replaceFileAtomically(sourcePath, targetPath string) error {
	return os.Rename(sourcePath, targetPath)
}

func secureInstalledExecutable(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("installed executable is not a regular file")
	}
	if err := os.Chown(path, 0, 0); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

func syncParentDirectory(path string) error {
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func syncRegularFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("path is not a regular file")
	}
	return file.Sync()
}

func installRemovalHelper(helperDirectory, helperPath string) error {
	if filepath.Clean(filepath.Dir(helperPath)) != filepath.Clean(helperDirectory) {
		return errors.New("removal helper path is outside its managed directory")
	}
	info, err := os.Lstat(helperDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("removal helper directory preparation failed")
	}
	if err := os.Chown(helperDirectory, 0, 0); err != nil {
		return fmt.Errorf("removal helper directory preparation failed: %w", err)
	}
	if err := os.Chmod(helperDirectory, 0o700); err != nil {
		return fmt.Errorf("removal helper directory preparation failed: %w", err)
	}
	sourcePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("removal helper source unavailable: %w", err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("removal helper source unavailable: %w", err)
	}
	defer source.Close()
	sourceInfo, err := source.Stat()
	if err != nil || !sourceInfo.Mode().IsRegular() {
		return errors.New("removal helper source is invalid")
	}

	temporary, err := os.CreateTemp(helperDirectory, ".removal-helper-installing-")
	if err != nil {
		return fmt.Errorf("removal helper creation failed: %w", err)
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		_ = temporary.Close()
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	written, err := io.Copy(temporary, source)
	if err != nil {
		return fmt.Errorf("removal helper copy failed: %w", err)
	}
	if written != sourceInfo.Size() {
		return errors.New("removal helper copy was incomplete")
	}
	if err := temporary.Chown(0, 0); err != nil {
		return fmt.Errorf("removal helper ownership failed: %w", err)
	}
	if err := temporary.Chmod(0o700); err != nil {
		return fmt.Errorf("removal helper permissions failed: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("removal helper sync failed: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("removal helper close failed: %w", err)
	}
	if err := os.Rename(temporaryPath, helperPath); err != nil {
		return fmt.Errorf("removal helper publication failed: %w", err)
	}
	keepTemporary = false
	if err := syncParentDirectory(helperPath); err != nil {
		return fmt.Errorf("removal helper directory sync failed: %w", err)
	}
	return nil
}

func removeFileIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// stageIdentityDirectoryForRemoval is the POSIX identity-generation commit
// boundary. The sensitive state is first reduced to marker + running helper,
// then the whole root is renamed to a deterministic sibling while the helper
// still owns the lifecycle and removal flocks. Stale waiters can no longer
// validate their old directory inode against the canonical name.
func stageIdentityDirectoryForRemoval(paths identityPaths, helperPath string) error {
	tombstone, err := identityRemovalTombstonePath(paths)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(tombstone); err == nil {
		return errors.New("terminal removal tombstone already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("terminal removal tombstone state is unavailable")
	}
	if err := prepareIdentityForFinalRemoval(paths, helperPath); err != nil {
		return err
	}
	if err := removeFileIfPresent(paths.gatewayID); err != nil {
		return fmt.Errorf("GatewayID final removal failed: %w", err)
	}
	if err := syncIdentityDirectory(paths.directory); err != nil {
		return err
	}
	source, err := os.Lstat(paths.directory)
	if err != nil || !source.IsDir() || source.Mode()&os.ModeSymlink != 0 {
		return errors.New("Gateway identity directory is invalid")
	}
	if err := os.Rename(paths.directory, tombstone); err != nil {
		return fmt.Errorf("Gateway identity removal staging failed: %w", err)
	}
	staged, err := os.Lstat(tombstone)
	if err != nil || !staged.IsDir() || staged.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(source, staged) {
		return errors.New("Gateway identity removal staging could not be verified")
	}
	if err := syncParentDirectory(paths.directory); err != nil {
		return fmt.Errorf("Gateway identity removal staging sync failed: %w", err)
	}
	return nil
}

func removeExactDirectory(path, expected string) error {
	if filepath.Clean(path) != filepath.Clean(expected) || filepath.Clean(expected) == "/" {
		return errors.New("refusing to remove an unexpected directory")
	}
	if err := os.RemoveAll(path); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
