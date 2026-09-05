//go:build linux

package main

import (
	"context"
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
	runController(ctx, credentials, resume)
	return nil
}
