package main

import (
	"context"
	"errors"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

const (
	retryDelay            = 90 * time.Second
	frameSilenceTimeout   = 90 * time.Second
	cloudOperationTimeout = 90 * time.Second
)

// clearStaleFramePackageState removes package data left by an earlier process
// before this service execution can park on credentials or control-plane
// availability. Identity, update, and terminal-removal state are siblings of
// frame-packages and are deliberately left untouched.
func clearStaleFramePackageState() error {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	if err := prepareIdentityWorkDirectory(paths); err != nil {
		return err
	}
	return removeFramePackageRoot(paths.workDirectory)
}

func startGateway(
	ctx context.Context,
	devices []deviceRecord,
	credentials *runtimeCredentials,
) (result error) {
	if credentials == nil {
		return errors.New("runtime credentials are unavailable")
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	if err := prepareIdentityWorkDirectory(paths); err != nil {
		return err
	}
	packageRoot, err := resetFramePackageRoot(paths.workDirectory)
	if err != nil {
		return err
	}
	defer func() {
		if err := removeFramePackageRoot(paths.workDirectory); result == nil {
			result = err
		}
	}()

	store, err := newPostgresStore(
		ctx,
		credentials.cloudPlatformTokenSource,
		credentials.databaseLoginTokenSource,
	)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer store.close()

	gcsClient, err := storage.NewClient(
		ctx,
		option.WithTokenSource(credentials.cloudPlatformTokenSource),
	)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("cloud client startup failed")
	}
	defer gcsClient.Close()

	runtimes := make([]*deviceRuntime, len(devices))
	for index := range devices {
		device := devices[index]
		packages, err := newFramePackageCache(
			framePackageDeviceDirectory(
				packageRoot,
				device.organisationID,
				device.siteID,
				device.id,
			),
			device.framePackageIntervalMinutes,
		)
		if err != nil {
			return err
		}
		runtimes[index] = newDeviceRuntime(device, packages)
	}

	var workers sync.WaitGroup
	workers.Add(2 * len(runtimes))
	if len(runtimes) != 0 {
		factStatement := runtimeFactStatement(len(runtimes))
		workers.Add(1)
		go func() {
			defer workers.Done()
			superviseRuntimeFacts(ctx, store, factStatement, runtimes)
		}()
	}
	for _, runtime := range runtimes {
		go func() {
			defer workers.Done()
			superviseDevice(ctx, runtime)
		}()
		go func() {
			defer workers.Done()
			superviseFramePackageUploader(ctx, gcsClient, runtime)
		}()
	}

	<-ctx.Done()
	workers.Wait()
	return nil
}

func superviseFramePackageUploader(
	ctx context.Context,
	client *storage.Client,
	runtime *deviceRuntime,
) {
	for {
		func() {
			defer func() {
				_ = recover()
			}()
			runFramePackageUploader(
				ctx,
				runtime.packages,
				framePackageRetryDelay(runtime.config.framePackageIntervalMinutes),
				func(
					uploadCtx context.Context,
					framePackage framePackageUpload,
				) (time.Time, error) {
					return uploadFramePackage(
						uploadCtx,
						client,
						runtime.config,
						framePackage,
					)
				},
				runtime.facts.observeUpload,
			)
		}()
		if ctx.Err() != nil || !waitContext(ctx, retryDelay) {
			return
		}
	}
}

func framePackageRetryDelay(intervalMinutes int) time.Duration {
	if intervalMinutes > 0 && intervalMinutes < 3 {
		return time.Duration(intervalMinutes) * time.Minute / 2
	}
	return retryDelay
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
