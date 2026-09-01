package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

const serviceAccountFilename = "sa.json"

func commission(ctx context.Context, siteID int64) (returnErr error) {
	credentialsJSON, err := os.ReadFile(serviceAccountFilename)
	if err != nil {
		return errors.New("sa.json unavailable")
	}

	store, err := newPostgresStore(ctx, credentialsJSON)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.close(); err != nil {
			fmt.Fprintln(os.Stderr, "WARN: gateway shutdown failed")
			if returnErr == nil {
				returnErr = err
			}
		}
	}()

	devices, err := store.loadDevices(ctx, siteID)
	if err != nil {
		return err
	}
	if len(devices) == 0 {
		return errors.New("no active devices")
	}

	gcsClient, err := storage.NewClient(ctx, option.WithCredentialsJSON(credentialsJSON))
	if err != nil {
		return errors.New("gateway startup failed")
	}
	credentialsJSON = nil
	defer func() {
		if err := gcsClient.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "WARN: gateway shutdown failed")
			if returnErr == nil {
				returnErr = errors.New("gateway shutdown failed")
			}
		}
	}()

	recordEvent(ctx, store, siteID, 0, eventGatewayStarted, "")
	runErr := runSiteSupervisor(ctx, siteID, devices, store, gcsClient)
	if ctx.Err() != nil && runErr == nil {
		recordEvent(context.Background(), store, siteID, 0, eventGatewayStopped, "")
		return nil
	}
	return runErr
}

func runSiteSupervisor(
	ctx context.Context,
	siteID int64,
	devices []deviceRecord,
	store *postgresStore,
	gcsClient *storage.Client,
) error {
	var workers sync.WaitGroup
	workers.Add(len(devices))
	for index := range devices {
		device := devices[index]
		devices[index] = deviceRecord{}
		go func() {
			defer workers.Done()
			runDevice(ctx, siteID, device, store, gcsClient)
		}()
	}
	workers.Wait()
	if ctx.Err() != nil {
		return nil
	}
	return errors.New("all device workers stopped")
}

func runDevice(
	ctx context.Context,
	siteID int64,
	device deviceRecord,
	store *postgresStore,
	gcsClient *storage.Client,
) {
	deviceID := device.id
	defer func() {
		if recover() != nil {
			recordDeviceFailure(ctx, store, siteID, deviceID, failureStageCrash)
		}
	}()

	recordEvent(ctx, store, siteID, deviceID, eventDeviceStarted, "")
	runtime := deviceRuntime{siteID: siteID, deviceID: deviceID, store: store}
	sourceURI, fps, runErr := runtime.prepare(device, gcsClient)
	device = deviceRecord{}
	if runErr == nil {
		runErr = runtime.run(ctx, sourceURI, fps)
	}

	shutdownContext := ctx
	if ctx.Err() != nil {
		shutdownContext = context.Background()
	}
	flushErr := runtime.flush(shutdownContext)

	if ctx.Err() != nil {
		if flushErr != nil {
			recordDeviceFailure(shutdownContext, store, siteID, deviceID, failureStageDatabase)
			return
		}
		recordEvent(shutdownContext, store, siteID, deviceID, eventDeviceStopped, "")
		return
	}
	if flushErr != nil {
		recordDeviceFailure(ctx, store, siteID, deviceID, failureStageDatabase)
		return
	}

	failure, ok := runErr.(deviceFailure)
	if !ok {
		failure = failureStageStream
	}
	recordDeviceFailure(ctx, store, siteID, deviceID, failure)
}

func recordDeviceFailure(
	ctx context.Context,
	store *postgresStore,
	siteID, deviceID int64,
	stage deviceFailure,
) {
	recordEvent(ctx, store, siteID, deviceID, eventDeviceFailed, stage)
	fmt.Fprintf(os.Stderr, "WARN: camera failed [device_id=%d]\n", deviceID)
}

func recordEvent(
	ctx context.Context,
	store *postgresStore,
	siteID, deviceID int64,
	eventType string,
	stage deviceFailure,
) {
	if ctx.Err() != nil {
		ctx = context.Background()
	}

	details := `{}`
	if stage != "" {
		details = `{"stage":"` + string(stage) + `"}`
	}
	var databaseDeviceID any
	if deviceID != 0 {
		databaseDeviceID = deviceID
	}

	_, err := store.database.ExecContext(
		ctx,
		`INSERT INTO public.gateway_logs (
    site_id,
    device_id,
    occurred_at,
    event_type,
    message,
    details
)
VALUES ($1, $2, $3, $4, $4, $5::jsonb)`,
		siteID,
		databaseDeviceID,
		time.Now().UTC(),
		eventType,
		details,
	)
	if err == nil {
		return
	}
	if deviceID == 0 {
		fmt.Fprintf(os.Stderr, "WARN: gateway log write failed [site_id=%d event=%s]\n", siteID, eventType)
		return
	}
	fmt.Fprintf(
		os.Stderr,
		"WARN: gateway log write failed [site_id=%d device_id=%d event=%s]\n",
		siteID,
		deviceID,
		eventType,
	)
}
