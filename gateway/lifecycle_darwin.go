//go:build darwin

package main

import (
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// Use a stable OS executable as the final launchd program. The custom
	// Gateway helper validates committed authority and performs sensitive
	// cleanup first; after it succeeds and unlinks itself, this wrapper can
	// idempotently finish deletion even on the next boot.
	finalizer := fmt.Sprintf(
		"set -e; if [ -x %s ]; then %s internal-remove; fi; if [ -e %s ] || [ -L %s ]; then test -f %s; test ! -e %s; test ! -L %s; test ! -e %s; test ! -L %s; /bin/rm -rf -- %s; fi; /bin/rm -f -- %s; /bin/rm -f -- %s; /bin/launchctl bootout %s",
		quotePOSIXShellArgument(helperPath),
		quotePOSIXShellArgument(helperPath),
		quotePOSIXShellArgument(gatewayIdentityDirectory),
		quotePOSIXShellArgument(gatewayIdentityDirectory),
		quotePOSIXShellArgument(filepath.Join(gatewayIdentityDirectory, removalPendingFilename)),
		quotePOSIXShellArgument(gatewayIdentityPath),
		quotePOSIXShellArgument(gatewayIdentityPath),
		quotePOSIXShellArgument(helperPath),
		quotePOSIXShellArgument(helperPath),
		quotePOSIXShellArgument(gatewayIdentityDirectory),
		quotePOSIXShellArgument(removalLaunchDaemonPath+".installing"),
		quotePOSIXShellArgument(removalLaunchDaemonPath),
		quotePOSIXShellArgument(removalLaunchDaemonTarget),
	)
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>%s</string>
    <key>ProgramArguments</key>
    <array>
        <string>/bin/sh</string>
        <string>-c</string>
        <string>%s</string>
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
`, removalLaunchDaemonLabel, html.EscapeString(finalizer))
}

func launchDaemonTargetLoaded(target string) (bool, error) {
	command := exec.Command("/bin/launchctl", "print", target)
	output, err := command.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) && launchctlPrintProvesAbsent(string(output), target) {
		return false, nil
	}
	return false, errors.New("LaunchDaemon state check returned an ambiguous failure")
}

func launchctlPrintProvesAbsent(output, target string) bool {
	normalized := strings.ToLower(strings.Join(strings.Fields(output), " "))
	label := strings.ToLower(strings.TrimPrefix(target, "system/"))
	if label == "" || strings.ContainsAny(label, " \t\r\n\"") {
		return false
	}
	canonical := []string{
		fmt.Sprintf("could not find service %s in domain for system", label),
		fmt.Sprintf("could not find service \"%s\" in domain for system", label),
	}
	for _, message := range canonical {
		if normalized == message || normalized == "bad request. "+message {
			return true
		}
	}
	return false
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

	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	helperPath := filepath.Join(paths.workDirectory, removalHelperFilename)
	if err := prepareIdentityForFinalRemoval(paths, helperPath); err != nil {
		return err
	}
	// GatewayID absence and helper absence together are the wrapper's narrow
	// proof that the validating helper crossed its successful cleanup boundary.
	// Either deletion may be retried after a crash; neither permits a stale
	// native wrapper to delete a different, still-committed identity.
	if err := deleteGatewayID(); err != nil {
		return fmt.Errorf("GatewayID final removal failed: %w", err)
	}
	if err := removeFileIfPresent(helperPath); err != nil {
		return fmt.Errorf("removal helper unlink failed: %w", err)
	}
	return syncIdentityDirectory(paths.workDirectory)
}

func platformRemovalStatePresent() (bool, error) {
	_, err := os.Lstat(removalLaunchDaemonPath)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, errors.New("terminal removal native state is unavailable")
	}
	loaded, err := launchDaemonTargetLoaded(removalLaunchDaemonTarget)
	if err != nil {
		return false, errors.New("terminal removal native state is unavailable")
	}
	// launchd retains the loaded job and its cached shell finalizer after the
	// plist is unlinked. Treat that in-memory job as committed native authority
	// until launchctl proves it absent, or it could delete a new commission.
	return darwinRemovalNativeState(false, loaded, nil)
}

func darwinRemovalNativeState(filePresent, loaded bool, probeErr error) (bool, error) {
	if filePresent {
		return true, nil
	}
	if probeErr != nil {
		return false, errors.New("terminal removal native state is unavailable")
	}
	return loaded, nil
}
