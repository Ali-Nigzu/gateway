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

	"golang.org/x/sys/unix"
)

func quotePOSIXShellArgument(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// acquireGatewayLifecycleLock serializes the short destructive/recovery
// boundary across accidental duplicate service processes. Lock the existing
// identity directory rather than adding another persistent state file. flock
// is released by the kernel if a process exits or crashes.
func acquireGatewayLifecycleLock(timeout time.Duration) (func(), error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return nil, err
	}
	return acquirePOSIXPathLock(paths.directory, timeout, "lifecycle")
}

// Downloads use the immutable GatewayID inode as a separate advisory lock so
// a slow body transfer never delays terminal-removal authority on the identity
// directory lock. Neither lock creates persistent state.
func acquireGatewayDownloadLock(timeout time.Duration) (func(), error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return nil, err
	}
	return acquirePOSIXPathLock(paths.gatewayID, timeout, "download")
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

func installedExecutablePath() (string, error) {
	return gatewayExecutablePath, nil
}

func installedFFmpegPath() (string, error) {
	return gatewayFFmpegPath, nil
}

func candidateExecutablePath() (string, error) {
	return gatewayExecutablePath + ".candidate", nil
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
	if err := ensureRootDirectory(helperDirectory, 0o700); err != nil {
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
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() {
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
	if written != info.Size() {
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
