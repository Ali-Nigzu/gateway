package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
)

const (
	retryDelay             = 90 * time.Second
	frameSilenceTimeout    = 90 * time.Second
	cloudOperationTimeout  = 90 * time.Second
	runtimeFactInterval    = 90 * time.Second
	serviceAccountFilename = "sa.json"
)

func startGateway(ctx context.Context, siteID int64) error {
	credentialsJSON, err := loadCredentialsJSON()
	if err != nil {
		return err
	}
	authCtx := context.WithValue(
		ctx,
		oauth2.HTTPClient,
		&http.Client{Timeout: cloudOperationTimeout},
	)

	store, err := newPostgresStore(authCtx, credentialsJSON)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	defer store.close()

	gcsClient, err := storage.NewClient(authCtx, option.WithCredentialsJSON(credentialsJSON))
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return errors.New("cloud client startup failed")
	}
	defer gcsClient.Close()

	devices, err := loadDevicesUntilReady(ctx, store, siteID)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

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

func loadCredentialsJSON() ([]byte, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, errors.New("sa.json unavailable")
	}
	credentialsJSON, err := os.ReadFile(filepath.Join(filepath.Dir(executable), serviceAccountFilename))
	if err != nil {
		return nil, errors.New("sa.json unavailable")
	}
	return credentialsJSON, nil
}

func loadDevicesUntilReady(
	ctx context.Context,
	store *postgresStore,
	siteID int64,
) ([]deviceRecord, error) {
	for {
		operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
		devices, err := store.loadDevices(operationCtx, siteID)
		cancel()
		if err == nil {
			return devices, nil
		}
		if ctx.Err() != nil || !waitContext(ctx, retryDelay) {
			return nil, ctx.Err()
		}
	}
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
