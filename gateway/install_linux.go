//go:build linux

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
	gatewayInstallDirectory = "/usr/local/libexec/camos-gateway"
	gatewayExecutablePath   = gatewayInstallDirectory + "/camos-gateway"
	gatewayFFmpegPath       = gatewayInstallDirectory + "/ffmpeg"

	systemdGatewayUnitName = "camos-gateway.service"
	systemdGatewayUnitPath = "/etc/systemd/system/" + systemdGatewayUnitName

	systemdStartTimeout = 2 * time.Minute
	systemdPollInterval = 250 * time.Millisecond
)

const systemdGatewayUnit = `[Unit]
Description=camOS Gateway
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStart=/usr/local/libexec/camos-gateway/camos-gateway service
Restart=always
RestartSec=30
TimeoutStopSec=120
KillMode=control-group
User=root
Group=root
UMask=0077

[Install]
WantedBy=multi-user.target
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

	unitSame, err := installedSystemdUnitMatches()
	if err != nil {
		return err
	}
	if !unitSame {
		if err := writeRootFileAtomically(systemdGatewayUnitPath, []byte(systemdGatewayUnit), 0o644); err != nil {
			return fmt.Errorf("systemd unit write failed: %w", err)
		}
		if err := runSystemctl("daemon-reload"); err != nil {
			return fmt.Errorf("systemd reload failed: %w", err)
		}
	} else if err := secureInstalledPackageFile(systemdGatewayUnitPath, 0o644); err != nil {
		return err
	}
	if err := runSystemctl("enable", systemdGatewayUnitName); err != nil {
		return fmt.Errorf("systemd enable failed: %w", err)
	}
	if unitSame {
		if err := runSystemctl("start", systemdGatewayUnitName); err != nil {
			return fmt.Errorf("systemd start failed: %w", err)
		}
	} else if err := runSystemctl("restart", systemdGatewayUnitName); err != nil {
		return fmt.Errorf("systemd restart failed: %w", err)
	}
	return waitForSystemdGatewayRunning()
}

func installedSystemdUnitMatches() (bool, error) {
	encoded, err := os.ReadFile(systemdGatewayUnitPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errors.New("systemd unit read failed")
	}
	return bytes.Equal(encoded, []byte(systemdGatewayUnit)), nil
}

func waitForSystemdGatewayRunning() error {
	deadline := time.Now().Add(systemdStartTimeout)
	for {
		command := exec.Command("/bin/systemctl", "is-active", "--quiet", systemdGatewayUnitName)
		if err := command.Run(); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("Gateway systemd service did not reach active state")
		}
		time.Sleep(systemdPollInterval)
	}
}

func runSystemctl(arguments ...string) error {
	command := exec.Command("/bin/systemctl", arguments...)
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
