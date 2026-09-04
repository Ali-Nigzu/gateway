//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

const (
	removalLaunchDaemonLabel  = "com.camos.gateway.removal"
	removalLaunchDaemonPath   = "/Library/LaunchDaemons/com.camos.gateway.removal.plist"
	removalLaunchDaemonTarget = "system/" + removalLaunchDaemonLabel
	removalHelperFilename     = "camos-gateway-removal"
)

func beginGatewayRemoval() error {
	if os.Geteuid() != 0 {
		return errors.New("terminal Gateway removal must run as root")
	}
	if err := markGatewayRemovalPending(); err != nil {
		return err
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	helperPath := filepath.Join(paths.workDirectory, removalHelperFilename)
	if err := installRemovalHelper(paths.workDirectory, helperPath); err != nil {
		return err
	}
	propertyList := removalLaunchDaemonPropertyList(helperPath)
	if err := writeRootFileAtomically(
		removalLaunchDaemonPath,
		[]byte(propertyList),
		0o600,
	); err != nil {
		return fmt.Errorf("removal LaunchDaemon definition write failed: %w", err)
	}

	loaded, err := launchDaemonTargetLoaded(removalLaunchDaemonTarget)
	if err != nil {
		return err
	}
	if !loaded {
		if err := runLaunchctl("bootstrap", "system", removalLaunchDaemonPath); err != nil {
			return fmt.Errorf("removal LaunchDaemon installation failed: %w", err)
		}
	}
	if err := runLaunchctl("kickstart", removalLaunchDaemonTarget); err != nil {
		return fmt.Errorf("removal LaunchDaemon start failed: %w", err)
	}
	return nil
}

func removalLaunchDaemonPropertyList(helperPath string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>internal-remove</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>5</integer>
    <key>Umask</key>
    <integer>63</integer>
</dict>
</plist>
`, removalLaunchDaemonLabel, helperPath)
}

func launchDaemonTargetLoaded(target string) (bool, error) {
	command := exec.Command("/bin/launchctl", "print", target)
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

func handleInternalPlatformCommand(arguments []string) (bool, error) {
	if len(arguments) != 1 || arguments[0] != "internal-remove" {
		return false, nil
	}
	return true, runDarwinRemovalHelper()
}

func runDarwinRemovalHelper() error {
	if os.Geteuid() != 0 {
		return errors.New("terminal Gateway removal helper must run as root")
	}
	pending, err := gatewayRemovalPending()
	if err != nil {
		return err
	}
	if !pending {
		return errors.New("terminal Gateway removal was not authorized")
	}

	loaded, err := launchDaemonTargetLoaded(launchDaemonTarget)
	if err != nil {
		return err
	}
	if loaded {
		if err := runLaunchctl("bootout", launchDaemonTarget); err != nil {
			return fmt.Errorf("Gateway LaunchDaemon removal failed: %w", err)
		}
	}
	if err := removeFileIfPresent(launchDaemonPath + ".installing"); err != nil {
		return fmt.Errorf("Gateway LaunchDaemon temporary definition removal failed: %w", err)
	}
	if err := removeFileIfPresent(launchDaemonPath); err != nil {
		return fmt.Errorf("Gateway LaunchDaemon definition removal failed: %w", err)
	}
	if err := syncParentDirectory(launchDaemonPath); err != nil {
		return fmt.Errorf("Gateway LaunchDaemon directory sync failed: %w", err)
	}
	if err := removeExactDirectory(gatewayInstallDirectory, "/usr/local/libexec/camos-gateway"); err != nil {
		return fmt.Errorf("Gateway installation removal failed: %w", err)
	}

	// Keep the supervised helper and durable marker until all permanent
	// application files are gone. Unlink the helper's definition immediately
	// before removing the state tree that contains the running helper itself.
	if err := removeFileIfPresent(removalLaunchDaemonPath + ".installing"); err != nil {
		return fmt.Errorf("removal LaunchDaemon temporary definition cleanup failed: %w", err)
	}
	if err := removeFileIfPresent(removalLaunchDaemonPath); err != nil {
		return fmt.Errorf("removal LaunchDaemon definition cleanup failed: %w", err)
	}
	if err := syncParentDirectory(removalLaunchDaemonPath); err != nil {
		return fmt.Errorf("removal LaunchDaemon directory sync failed: %w", err)
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	if err := removeExactDirectory(paths.directory, gatewayIdentityDirectory); err != nil {
		return fmt.Errorf("Gateway identity removal failed: %w", err)
	}

	// bootout terminates this final transient job. At this point its plist,
	// executable, marker, permanent service, identity and installation are gone.
	command := exec.Command("/bin/launchctl", "bootout", removalLaunchDaemonTarget)
	if err := command.Start(); err != nil {
		return fmt.Errorf("removal LaunchDaemon unload failed: %w", err)
	}
	return nil
}
