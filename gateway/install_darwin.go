//go:build darwin

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	launchDaemonLabel  = "com.camos.gateway"
	launchDaemonPath   = "/Library/LaunchDaemons/com.camos.gateway.plist"
	launchDaemonTarget = "system/" + launchDaemonLabel

	gatewayInstallDirectory = "/usr/local/libexec/camos-gateway"
	gatewayExecutablePath   = gatewayInstallDirectory + "/camos-gateway"
	gatewayFFmpegPath       = gatewayInstallDirectory + "/ffmpeg"

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
    <key>ProcessType</key>
    <string>Background</string>
    <key>ThrottleInterval</key>
    <integer>5</integer>
    <key>Umask</key>
    <integer>63</integer>
</dict>
</plist>
`

func installService() error {
	if os.Geteuid() != 0 {
		return errors.New("Gateway service installation must be run as root")
	}
	if err := validateEmbeddedRelease(); err != nil {
		return err
	}
	if err := installPackageIfAbsent(); err != nil {
		return err
	}
	if err := ensureEmbeddedFFmpeg(gatewayFFmpegPath); err != nil {
		return err
	}

	loaded, err := launchDaemonLoaded()
	if err != nil {
		return err
	}
	plistSame, err := installedLaunchDaemonMatches()
	if err != nil {
		return err
	}
	if loaded && !plistSame {
		if err := runLaunchctl("bootout", launchDaemonTarget); err != nil {
			return fmt.Errorf("LaunchDaemon stop failed: %w", err)
		}
		loaded = false
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

func writeLaunchDaemonPropertyList() error {
	if err := writeRootFileAtomically(
		launchDaemonPath,
		[]byte(launchDaemonPropertyList),
		0o644,
	); err != nil {
		return fmt.Errorf("LaunchDaemon definition write failed: %w", err)
	}
	return nil
}

func secureLaunchDaemonPropertyList() error {
	info, err := os.Lstat(launchDaemonPath)
	if err != nil {
		return fmt.Errorf("LaunchDaemon definition inspection failed: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("LaunchDaemon definition is not a regular file")
	}
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
