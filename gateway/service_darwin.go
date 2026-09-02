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
	gatewayID, err := loadGatewayID()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	resume := make(chan struct{}, 1)
	powerWatcher, err := startPowerResumeWatcher(resume)
	if err != nil {
		return fmt.Errorf("power notification registration failed: %w", err)
	}
	defer powerWatcher.stop()

	runController(ctx, gatewayID, resume)
	return nil
}
