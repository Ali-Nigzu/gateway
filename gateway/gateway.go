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
	runtimeFactInterval   = 90 * time.Second
)

func startGateway(
	ctx context.Context,
	devices []deviceRecord,
	credentials *runtimeCredentials,
) error {
	if credentials == nil {
		return errors.New("runtime credentials are unavailable")
	}
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
		runtimes[index] = newDeviceRuntime(devices[index])
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
			superviseDeviceUploader(ctx, gcsClient, runtime)
		}()
	}

	<-ctx.Done()
	workers.Wait()
	return nil
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
