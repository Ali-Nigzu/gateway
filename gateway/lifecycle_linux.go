//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	systemdRemovalUnitName = "camos-gateway-remove.service"
	systemdRemovalUnitPath = "/etc/systemd/system/" + systemdRemovalUnitName
	linuxRemovalHelperName = "camos-gateway-removal"
)

func beginGatewayRemoval() error {
	if os.Geteuid() != 0 {
		return errors.New("terminal Gateway removal must run as root")
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	required, err := posixRemovalHelperPreparationRequired(paths)
	if err != nil {
		return err
	}
	if !required {
		return nil
	}
	releaseRemoval, err := acquireGatewayRemovalLock(lifecycleOperationLockWait)
	if err != nil {
		// A finalizer may have removed the marker while this duplicate actor
		// waited to join removal. Treat only a freshly proven terminal state as
		// successful completion; every ambiguous state remains fail-closed.
		if required, retryErr := posixRemovalHelperPreparationRequired(paths); retryErr == nil && !required {
			return nil
		}
		return err
	}
	defer releaseRemoval()
	required, err = posixRemovalHelperPreparationRequired(paths)
	if err != nil {
		return err
	}
	if !required {
		return nil
	}
	if err := prepareIdentityWorkDirectory(paths); err != nil {
		return err
	}
	helperPath := filepath.Join(paths.workDirectory, linuxRemovalHelperName)
	if err := installRemovalHelper(paths.workDirectory, helperPath); err != nil {
		return err
	}
	unit, err := systemdRemovalUnit(helperPath)
	if err != nil {
		return err
	}
	if err := writeRootFileAtomically(systemdRemovalUnitPath, []byte(unit), 0o600); err != nil {
		return fmt.Errorf("removal systemd unit write failed: %w", err)
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return fmt.Errorf("removal systemd reload failed: %w", err)
	}
	if err := runSystemctl("enable", systemdRemovalUnitName); err != nil {
		return fmt.Errorf("removal systemd enable failed: %w", err)
	}
	if err := runSystemctl("start", "--no-block", systemdRemovalUnitName); err != nil {
		return fmt.Errorf("removal systemd start failed: %w", err)
	}
	return nil
}

func systemdRemovalUnit(helperPath string) (string, error) {
	// systemd launches a stable OS executable rather than the transient Gateway
	// copy directly. The helper atomically retires the canonical identity root
	// while holding both flocks; this stable wrapper owns only tombstone and
	// native-unit cleanup after that generation boundary.
	paths, err := resolveIdentityPaths()
	if err != nil {
		return "", err
	}
	prefix, err := posixRemovalFinalizerPrefix(paths, helperPath)
	if err != nil {
		return "", err
	}
	finalizer := prefix + fmt.Sprintf(
		"; /bin/rm -f -- %s; /bin/rm -f -- %s; /bin/rm -f -- %s; /bin/systemctl daemon-reload",
		quotePOSIXShellArgument(systemdRemovalUnitPath+".installing"),
		quotePOSIXShellArgument(systemdRemovalUnitPath),
		quotePOSIXShellArgument(filepath.Join(filepath.Dir(systemdRemovalUnitPath), "multi-user.target.wants", systemdRemovalUnitName)),
	)
	return fmt.Sprintf(`[Unit]
Description=Complete camOS Gateway terminal removal
After=local-fs.target

[Service]
Type=oneshot
ExecStart=/bin/sh -c %s
Restart=on-failure
RestartSec=5
User=root
Group=root
UMask=0077

[Install]
WantedBy=multi-user.target
`, quotePOSIXShellArgument(finalizer)), nil
}

func handleInternalPlatformCommand(arguments []string) (bool, error) {
	if len(arguments) != 1 || arguments[0] != "internal-remove" {
		return false, nil
	}
	return true, runLinuxRemovalHelper()
}

func runLinuxRemovalHelper() error {
	if os.Geteuid() != 0 {
		return errors.New("terminal Gateway removal helper must run as root")
	}
	releaseLifecycle, err := acquireGatewayLifecycleLock(lifecycleOperationLockWait)
	if err != nil {
		return err
	}
	defer releaseLifecycle()
	releaseRemoval, err := acquireGatewayRemovalLock(lifecycleOperationLockWait)
	if err != nil {
		return err
	}
	defer releaseRemoval()
	pending, err := gatewayRemovalPending()
	if err != nil {
		return err
	}
	if !pending {
		return errors.New("terminal Gateway removal was not authorized")
	}

	// Disable first so Restart=always cannot recreate the permanent process.
	if err := disableSystemdUnitIdempotently(systemdGatewayUnitName, systemdGatewayUnitPath, true); err != nil {
		return fmt.Errorf("Gateway systemd service disable failed: %w", err)
	}
	if err := removeFileIfPresent(systemdGatewayUnitPath + ".installing"); err != nil {
		return fmt.Errorf("Gateway systemd temporary unit removal failed: %w", err)
	}
	if err := removeFileIfPresent(systemdGatewayUnitPath); err != nil {
		return fmt.Errorf("Gateway systemd unit removal failed: %w", err)
	}
	if err := syncParentDirectory(systemdGatewayUnitPath); err != nil {
		return fmt.Errorf("Gateway systemd unit directory sync failed: %w", err)
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return fmt.Errorf("Gateway systemd reload failed: %w", err)
	}
	if err := removeExactDirectory(gatewayInstallDirectory, "/usr/local/libexec/camos-gateway"); err != nil {
		return fmt.Errorf("Gateway installation removal failed: %w", err)
	}

	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	helperPath := filepath.Join(paths.workDirectory, linuxRemovalHelperName)
	return stageIdentityDirectoryForRemoval(paths, helperPath)
}

func platformRemovalStatePresent() (bool, error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return false, err
	}
	if present, err := posixRemovalTombstonePresent(paths); err != nil || present {
		return present, err
	}
	_, err = os.Lstat(systemdRemovalUnitPath)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, errors.New("terminal removal native state is unavailable")
	}
	missing, err := systemdUnitLoadStateNotFound(systemdRemovalUnitName)
	if err != nil {
		return false, errors.New("terminal removal native state is unavailable")
	}
	// systemd retains an in-memory unit after its unit file is unlinked until a
	// successful daemon-reload/garbage collection. That cached finalizer is still
	// destructive authority and must block commissioning and normal startup.
	return linuxRemovalNativeState(false, !missing, nil)
}

func linuxRemovalNativeState(filePresent, loaded bool, probeErr error) (bool, error) {
	if filePresent {
		return true, nil
	}
	if probeErr != nil {
		return false, errors.New("terminal removal native state is unavailable")
	}
	return loaded, nil
}

func disableSystemdUnitIdempotently(unitName, unitPath string, stop bool) error {
	arguments := []string{"disable"}
	if stop {
		arguments = append(arguments, "--now")
	}
	arguments = append(arguments, unitName)
	if err := runSystemctl(arguments...); err == nil {
		return nil
	}
	if _, statErr := os.Lstat(unitPath); statErr == nil {
		return errors.New("systemd unit still exists after disable failure")
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return errors.New("systemd unit state is unavailable")
	}
	missing, err := systemdUnitLoadStateNotFound(unitName)
	if err != nil || !missing {
		return errors.New("systemd unit absence could not be verified")
	}
	return nil
}

func systemdUnitLoadStateNotFound(unitName string) (bool, error) {
	command := exec.Command(
		"/bin/systemctl",
		"show",
		"--property=LoadState",
		"--value",
		unitName,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return false, err
	}
	return systemdLoadStateNotFound(strings.TrimSpace(string(output)))
}

func systemdLoadStateNotFound(state string) (bool, error) {
	switch state {
	case "not-found":
		return true, nil
	case "loaded", "masked", "bad-setting", "error", "merged":
		return false, nil
	default:
		return false, errors.New("systemd returned an ambiguous unit load state")
	}
}
