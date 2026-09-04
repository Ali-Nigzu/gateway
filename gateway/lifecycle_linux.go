//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	if err := markGatewayRemovalPending(); err != nil {
		return err
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	helperPath := filepath.Join(paths.workDirectory, linuxRemovalHelperName)
	if err := installRemovalHelper(paths.workDirectory, helperPath); err != nil {
		return err
	}
	unit := systemdRemovalUnit(helperPath)
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

func systemdRemovalUnit(helperPath string) string {
	return fmt.Sprintf(`[Unit]
Description=Complete camOS Gateway terminal removal
After=local-fs.target

[Service]
Type=oneshot
ExecStart=%s internal-remove
Restart=on-failure
RestartSec=5
User=root
Group=root
UMask=0077

[Install]
WantedBy=multi-user.target
`, helperPath)
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
	pending, err := gatewayRemovalPending()
	if err != nil {
		return err
	}
	if !pending {
		return errors.New("terminal Gateway removal was not authorized")
	}

	// Disable first so Restart=always cannot recreate the permanent process.
	if err := runSystemctl("disable", "--now", systemdGatewayUnitName); err != nil {
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
	if err := removeExactDirectory(paths.directory, "/var/lib/camos-gateway"); err != nil {
		return fmt.Errorf("Gateway identity removal failed: %w", err)
	}
	if err := runSystemctl("disable", systemdRemovalUnitName); err != nil {
		return fmt.Errorf("removal systemd unit disable failed: %w", err)
	}
	if err := removeFileIfPresent(systemdRemovalUnitPath + ".installing"); err != nil {
		return fmt.Errorf("removal systemd temporary unit cleanup failed: %w", err)
	}
	if err := removeFileIfPresent(systemdRemovalUnitPath); err != nil {
		return fmt.Errorf("removal systemd unit cleanup failed: %w", err)
	}
	if err := syncParentDirectory(systemdRemovalUnitPath); err != nil {
		return fmt.Errorf("removal systemd unit directory sync failed: %w", err)
	}
	if err := runSystemctl("daemon-reload"); err != nil {
		return fmt.Errorf("final systemd reload failed: %w", err)
	}
	return nil
}
