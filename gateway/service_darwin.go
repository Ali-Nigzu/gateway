//go:build darwin

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

const ffmpegExecutableName = "ffmpeg"

func runService() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pending, err := gatewayRemovalPending()
	if err != nil {
		return err
	}
	if pending {
		return beginGatewayRemoval()
	}
	if err := clearStaleFramePackageState(); err != nil {
		return err
	}
	ffmpegPath, err := installedFFmpegPath()
	if err != nil {
		return err
	}
	if err := ensureEmbeddedFFmpeg(ffmpegPath); err != nil {
		return err
	}
	credentials, err := newRuntimeCredentials(ctx)
	if err != nil {
		return err
	}

	resume := make(chan struct{}, 1)
	powerWatcher, err := startPowerResumeWatcher(resume)
	if err != nil {
		return fmt.Errorf("power notification registration failed: %w", err)
	}
	defer powerWatcher.stop()

	runController(ctx, credentials, resume)
	return nil
}
