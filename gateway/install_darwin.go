//go:build darwin

package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	launchDaemonLabel  = "com.camos.gateway"
	launchDaemonPath   = "/Library/LaunchDaemons/com.camos.gateway.plist"
	launchDaemonTarget = "system/" + launchDaemonLabel

	gatewayInstallDirectory = "/usr/local/libexec/camos-gateway"
	gatewayExecutablePath   = gatewayInstallDirectory + "/camos-gateway"
	gatewayFFmpegPath       = gatewayInstallDirectory + "/ffmpeg"
	gatewayCredentialsPath  = gatewayInstallDirectory + "/sa.json"

	launchDaemonStartTimeout = 2 * time.Minute
	launchDaemonPollInterval = 250 * time.Millisecond
)

const launchDaemonPropertyList = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.camos.gateway</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/libexec/camos-gateway/camos-gateway</string>
        <string>service</string>
    </array>
    <key>KeepAlive</key>
    <true/>
</dict>
</plist>
`

func installService(gatewayID uuid.UUID) error {
	definitionPresent, err := launchDaemonDefinitionPresent()
	if err != nil {
		return err
	}
	loaded, err := launchDaemonLoaded()
	if err != nil {
		return err
	}
	if !definitionPresent && !loaded {
		if err := installPackageIfAbsent(); err != nil {
			return err
		}
	}

	identitySame, err := installedGatewayIdentityMatches(gatewayID)
	if err != nil {
		return err
	}
	plistSame, err := installedLaunchDaemonMatches()
	if err != nil {
		return err
	}

	if loaded && (!identitySame || !plistSame) {
		if err := runLaunchctl("bootout", launchDaemonTarget); err != nil {
			return fmt.Errorf("LaunchDaemon stop failed: %w", err)
		}
		loaded = false
	}

	if !identitySame {
		if err := saveGatewayID(gatewayID); err != nil {
			return err
		}
	} else if err := secureGatewayIdentity(); err != nil {
		return err
	}

	if !plistSame {
		if err := writeLaunchDaemonPropertyList(); err != nil {
			return err
		}
	} else if err := secureLaunchDaemonPropertyList(); err != nil {
		return err
	}

	if err := runLaunchctl("enable", launchDaemonTarget); err != nil {
		return fmt.Errorf("LaunchDaemon enable failed: %w", err)
	}
	if !loaded {
		if err := runLaunchctl("bootstrap", "system", launchDaemonPath); err != nil {
			return fmt.Errorf("LaunchDaemon installation failed: %w", err)
		}
	}
	if err := runLaunchctl("kickstart", launchDaemonTarget); err != nil {
		return fmt.Errorf("LaunchDaemon start failed: %w", err)
	}
	return waitForLaunchDaemonRunning()
}

func installPackageIfAbsent() error {
	if err := os.MkdirAll(gatewayInstallDirectory, 0o755); err != nil {
		return fmt.Errorf("Gateway installation directory creation failed: %w", err)
	}
	if err := os.Chown(gatewayInstallDirectory, 0, 0); err != nil {
		return fmt.Errorf("Gateway installation directory ownership failed: %w", err)
	}
	if err := os.Chmod(gatewayInstallDirectory, 0o755); err != nil {
		return fmt.Errorf("Gateway installation directory permissions failed: %w", err)
	}

	executablePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("Gateway executable path unavailable: %w", err)
	}
	sourceDirectory := filepath.Dir(executablePath)
	packageFiles := []struct {
		source string
		target string
		mode   os.FileMode
	}{
		{source: executablePath, target: gatewayExecutablePath, mode: 0o755},
		{source: filepath.Join(sourceDirectory, ffmpegExecutableName), target: gatewayFFmpegPath, mode: 0o755},
		{source: filepath.Join(sourceDirectory, serviceAccountFilename), target: gatewayCredentialsPath, mode: 0o600},
	}

	for _, packageFile := range packageFiles {
		if err := copyPackageFileIfAbsent(packageFile.source, packageFile.target, packageFile.mode); err != nil {
			return err
		}
	}
	return nil
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
	if err := os.Link(temporaryPath, targetPath); err != nil {
		targetInfo, inspectionErr := os.Lstat(targetPath)
		if inspectionErr == nil {
			if !targetInfo.Mode().IsRegular() {
				return fmt.Errorf("installed package path is not a regular file: %s", targetPath)
			}
			if err := secureInstalledPackageFile(targetPath, mode); err != nil {
				return err
			}
			return removePackageInstallingFile(temporaryPath)
		}
		if !errors.Is(inspectionErr, os.ErrNotExist) {
			return fmt.Errorf("installed package path inspection failed for %s: %w", targetPath, inspectionErr)
		}
		return fmt.Errorf("package publication failed for %s: %w", targetPath, err)
	}
	if err := removePackageInstallingFile(temporaryPath); err != nil {
		return err
	}
	directory, err := os.Open(filepath.Dir(targetPath))
	if err != nil {
		return fmt.Errorf("package directory sync failed for %s: %w", targetPath, err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("package directory sync failed for %s: %w", targetPath, err)
	}
	return nil
}

func secureInstalledPackageFile(path string, mode os.FileMode) error {
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

func installedGatewayIdentityMatches(gatewayID uuid.UUID) (bool, error) {
	encoded, err := os.ReadFile(gatewayIdentityPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("GatewayID read failed: %w", err)
	}
	installedGatewayID, err := uuid.Parse(string(encoded))
	if err != nil {
		return false, nil
	}
	return installedGatewayID == gatewayID, nil
}

func installedLaunchDaemonMatches() (bool, error) {
	encoded, err := os.ReadFile(launchDaemonPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("LaunchDaemon definition read failed: %w", err)
	}
	return bytes.Equal(encoded, []byte(launchDaemonPropertyList)), nil
}

func launchDaemonDefinitionPresent() (bool, error) {
	_, err := os.Lstat(launchDaemonPath)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("LaunchDaemon definition inspection failed: %w", err)
}

func writeLaunchDaemonPropertyList() error {
	temporaryPath := launchDaemonPath + ".installing"
	if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("LaunchDaemon temporary file cleanup failed: %w", err)
	}
	temporary, err := os.OpenFile(
		temporaryPath,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL,
		0o644,
	)
	if err != nil {
		return fmt.Errorf("LaunchDaemon definition write failed: %w", err)
	}
	keepTemporary := true
	defer func() {
		if keepTemporary {
			os.Remove(temporaryPath)
		}
	}()

	if err := temporary.Chown(0, 0); err != nil {
		temporary.Close()
		return fmt.Errorf("LaunchDaemon definition ownership failed: %w", err)
	}
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return fmt.Errorf("LaunchDaemon definition permissions failed: %w", err)
	}
	if _, err := temporary.WriteString(launchDaemonPropertyList); err != nil {
		temporary.Close()
		return fmt.Errorf("LaunchDaemon definition write failed: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("LaunchDaemon definition sync failed: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("LaunchDaemon definition close failed: %w", err)
	}
	if err := os.Rename(temporaryPath, launchDaemonPath); err != nil {
		return fmt.Errorf("LaunchDaemon definition replacement failed: %w", err)
	}
	keepTemporary = false
	return nil
}

func secureLaunchDaemonPropertyList() error {
	if err := os.Chown(launchDaemonPath, 0, 0); err != nil {
		return fmt.Errorf("LaunchDaemon definition ownership failed: %w", err)
	}
	if err := os.Chmod(launchDaemonPath, 0o644); err != nil {
		return fmt.Errorf("LaunchDaemon definition permissions failed: %w", err)
	}
	return nil
}

func launchDaemonLoaded() (bool, error) {
	command := exec.Command("/bin/launchctl", "print", launchDaemonTarget)
	_, err := command.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return false, nil
	}
	return false, fmt.Errorf("LaunchDaemon state check failed: %w", err)
}

func waitForLaunchDaemonRunning() error {
	deadline := time.Now().Add(launchDaemonStartTimeout)
	for {
		command := exec.Command("/bin/launchctl", "print", launchDaemonTarget)
		output, err := command.CombinedOutput()
		if err == nil && bytes.Contains(output, []byte("state = running")) {
			return nil
		}
		var exitError *exec.ExitError
		if err != nil && !errors.As(err, &exitError) {
			return fmt.Errorf("LaunchDaemon state check failed: %w", err)
		}
		if time.Now().After(deadline) {
			return errors.New("LaunchDaemon did not reach running state")
		}
		time.Sleep(launchDaemonPollInterval)
	}
}

func runLaunchctl(arguments ...string) error {
	command := exec.Command("/bin/launchctl", arguments...)
	output, err := command.CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}
