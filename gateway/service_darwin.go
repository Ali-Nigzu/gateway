//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const ffmpegExecutableName = "ffmpeg"

func runService() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pending, err := posixRemovalPending()
	if err != nil {
		return err
	}
	if pending {
		return beginGatewayRemoval()
	}
	gatewayID, err := loadGatewayID()
	if err != nil {
		return err
	}
	handoff, startupUpdateFailure := reconcileUpdateStateAtStartup(gatewayID)
	if handoff {
		return nil
	}
	if err := clearStaleFramePackageState(); err != nil {
		return recoverPOSIXCandidateStartupFailure(gatewayID, err)
	}
	ffmpegPath, err := installedFFmpegPath()
	if err != nil {
		return recoverPOSIXCandidateStartupFailure(gatewayID, err)
	}
	if err := ensureEmbeddedFFmpeg(ffmpegPath); err != nil {
		return recoverPOSIXCandidateStartupFailure(gatewayID, err)
	}
	credentials, err := newRuntimeCredentials(ctx)
	if err != nil {
		return recoverPOSIXCandidateStartupFailure(gatewayID, err)
	}
	if credentials.gatewayID != gatewayID {
		return recoverPOSIXCandidateStartupFailure(
			gatewayID,
			errors.New("GatewayID changed during runtime startup"),
		)
	}

	resume := make(chan struct{}, 1)
	powerWatcher, err := startPowerResumeWatcher(resume)
	if err != nil {
		return recoverPOSIXCandidateStartupFailure(
			gatewayID,
			fmt.Errorf("power notification registration failed: %w", err),
		)
	}
	defer powerWatcher.stop()

	runController(ctx, credentials, resume, startupUpdateFailure)
	return nil
}
