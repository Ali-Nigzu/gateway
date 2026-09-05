//go:build darwin || linux

package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// installPackageIfAbsent installs only the complete Gateway executable. FFmpeg
// is reconciled separately from the payload embedded in that executable.
func installPackageIfAbsent() error {
	if err := ensureRootDirectory(gatewayInstallDirectory, 0o755); err != nil {
		return fmt.Errorf("Gateway installation directory preparation failed: %w", err)
	}

	executablePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("Gateway executable path unavailable: %w", err)
	}
	return copyPackageFileIfAbsent(executablePath, gatewayExecutablePath, 0o755)
}

func ensureRootDirectory(path string, mode os.FileMode) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("path is not a directory: %s", path)
	}
	if err := os.Chown(path, 0, 0); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func copyPackageFileIfAbsent(sourcePath, targetPath string, mode os.FileMode) error {
	targetInfo, err := os.Lstat(targetPath)
	if err == nil {
		if !targetInfo.Mode().IsRegular() {
			return fmt.Errorf("installed package path is not a regular file: %s", targetPath)
		}
		if err := secureInstalledPackageFile(targetPath, mode); err != nil {
			return err
		}
		return removePackageInstallingFile(targetPath + ".installing")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("installed package path inspection failed for %s: %w", targetPath, err)
	}

	temporaryPath := targetPath + ".installing"
	if err := removePackageInstallingFile(temporaryPath); err != nil {
		return err
	}

	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("package source unavailable at %s: %w", sourcePath, err)
	}
	defer source.Close()
	if sourceInfo, err := source.Stat(); err != nil {
		return fmt.Errorf("package source inspection failed for %s: %w", sourcePath, err)
	} else if !sourceInfo.Mode().IsRegular() {
		return fmt.Errorf("package source is not a regular file: %s", sourcePath)
	}

	temporary, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("package temporary file creation failed for %s: %w", targetPath, err)
	}
	keepTemporary := true
	defer func() {
		_ = temporary.Close()
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	if _, err := io.Copy(temporary, source); err != nil {
		return fmt.Errorf("package installation failed for %s: %w", targetPath, err)
	}
	if err := temporary.Chown(0, 0); err != nil {
		return fmt.Errorf("package ownership failed for %s: %w", targetPath, err)
	}
	if err := temporary.Chmod(mode); err != nil {
		return fmt.Errorf("package permissions failed for %s: %w", targetPath, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("package sync failed for %s: %w", targetPath, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("package close failed for %s: %w", targetPath, err)
	}

	// Linking publishes without replacing a package installed by a concurrent
	// commissioning attempt.
	if err := os.Link(temporaryPath, targetPath); err != nil {
		targetInfo, inspectionErr := os.Lstat(targetPath)
		if inspectionErr == nil {
			if !targetInfo.Mode().IsRegular() {
				return fmt.Errorf("installed package path is not a regular file: %s", targetPath)
			}
			if err := secureInstalledPackageFile(targetPath, mode); err != nil {
				return err
			}
			if err := removePackageInstallingFile(temporaryPath); err != nil {
				return err
			}
			keepTemporary = false
			return nil
		}
		if !errors.Is(inspectionErr, os.ErrNotExist) {
			return fmt.Errorf("installed package path inspection failed for %s: %w", targetPath, inspectionErr)
		}
		return fmt.Errorf("package publication failed for %s: %w", targetPath, err)
	}
	if err := removePackageInstallingFile(temporaryPath); err != nil {
		return err
	}
	keepTemporary = false
	if err := syncParentDirectory(targetPath); err != nil {
		return fmt.Errorf("package directory sync failed for %s: %w", targetPath, err)
	}
	return nil
}

func secureInstalledPackageFile(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("installed package inspection failed for %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("installed package path is not a regular file: %s", path)
	}
	if err := os.Chown(path, 0, 0); err != nil {
		return fmt.Errorf("installed package ownership failed for %s: %w", path, err)
	}
	if err := os.Chmod(path, mode); err != nil {
		return fmt.Errorf("installed package permissions failed for %s: %w", path, err)
	}
	return nil
}

func removePackageInstallingFile(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("package temporary file cleanup failed for %s: %w", path, err)
	}
	return nil
}

func writeRootFileAtomically(path string, contents []byte, mode os.FileMode) error {
	if err := ensureRootDirectory(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporaryPath := path + ".installing"
	if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	keepTemporary := true
	defer func() {
		_ = temporary.Close()
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(contents); err != nil {
		return err
	}
	if err := temporary.Chown(0, 0); err != nil {
		return err
	}
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	keepTemporary = false
	return syncParentDirectory(path)
}
