package connect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"cloud.google.com/go/storage"
)

const (
	serviceAccountFilename       = "sa.json"
	frameSeenPersistenceInterval = time.Minute
	shutdownOperationTimeout     = 5 * time.Second
)

type deviceFailure struct {
	stage string
}

func (deviceFailure) Error() string {
	return "camera failed"
}

// Commission loads the requested Site from Postgres and runs one independent
// camera worker for every enabled Device until cancellation or until all
// workers terminate.
func Commission(ctx context.Context, siteID int64) (returnErr error) {
	store, err := newPostgresStore(ctx, serviceAccountFilename)
	if err != nil {
		return err
	}
	defer func() {
		if err := store.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "WARN: gateway shutdown failed")
			returnErr = errors.Join(returnErr, errors.New("gateway shutdown failed"))
		}
	}()

	site, devices, err := store.LoadSite(ctx, siteID)
	if err != nil || site.status != "enabled" {
		return errors.New("site load failed")
	}

	activeDevices := make([]deviceRecord, 0, len(devices))
	for _, device := range devices {
		if device.status == "enabled" {
			activeDevices = append(activeDevices, device)
		}
	}
	if len(activeDevices) == 0 {
		return errors.New("no active devices")
	}

	gcsClient, err := newStorageClient(ctx, serviceAccountFilename)
	if err != nil {
		return err
	}
	defer func() {
		if err := gcsClient.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "WARN: gateway shutdown failed")
			returnErr = errors.Join(returnErr, errors.New("gateway shutdown failed"))
		}
	}()

	recordEvent(ctx, store, gatewayEvent{
		siteID: site.id, occurredAt: time.Now().UTC(),
		eventType: eventGatewayStarted, message: "Gateway started",
	})
	fmt.Printf("Gateway started [site_id=%d devices=%d]\n", site.id, len(activeDevices))

	runErr := runSiteSupervisor(ctx, site.id, activeDevices, store, gcsClient)
	if ctx.Err() != nil && runErr == nil {
		recordEvent(ctx, store, gatewayEvent{
			siteID: site.id, occurredAt: time.Now().UTC(),
			eventType: eventGatewayStopped, message: "Gateway stopped",
		})
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
	results := make(chan struct{}, len(devices))
	for _, device := range devices {
		device := device
		go func() {
			runDeviceWorker(ctx, siteID, device, store, gcsClient)
			results <- struct{}{}
		}()
	}

	for range devices {
		<-results
	}
	if ctx.Err() != nil {
		return nil
	}
	return errors.New("all device workers stopped")
}

func runDeviceWorker(
	ctx context.Context,
	siteID int64,
	device deviceRecord,
	store *postgresStore,
	gcsClient *storage.Client,
) {
	defer func() {
		if recover() != nil {
			recordDeviceFailure(ctx, store, siteID, device.id, failureStageCrash)
		}
	}()

	recordEvent(ctx, store, gatewayEvent{
		siteID: siteID, deviceID: &device.id, occurredAt: time.Now().UTC(),
		eventType: eventDeviceStarted, message: "Device started",
	})

	config, err := cameraConfigFromDevice(device)
	if err != nil {
		recordDeviceFailure(ctx, store, siteID, device.id, failureStageStream)
		return
	}

	observer := &deviceObserver{siteID: siteID, deviceID: device.id, store: store}
	runErr := connectCamera(ctx, config, gcsClient, observer)

	flushContext := ctx
	flushCancel := func() {}
	if ctx.Err() != nil {
		flushContext, flushCancel = context.WithTimeout(context.Background(), shutdownOperationTimeout)
	}
	flushErr := observer.flush(flushContext)
	flushCancel()

	if ctx.Err() != nil {
		if flushErr != nil {
			recordDeviceFailure(ctx, store, siteID, device.id, failureStageDatabase)
			return
		}
		recordEvent(ctx, store, gatewayEvent{
			siteID: siteID, deviceID: &device.id, occurredAt: time.Now().UTC(),
			eventType: eventDeviceStopped, message: "Device stopped",
		})
		return
	}

	if flushErr != nil {
		recordDeviceFailure(ctx, store, siteID, device.id, failureStageDatabase)
		return
	}

	stage := failureStageStream
	var failure deviceFailure
	if errors.As(runErr, &failure) {
		stage = failure.stage
	}
	recordDeviceFailure(ctx, store, siteID, device.id, stage)
}

func recordDeviceFailure(ctx context.Context, store *postgresStore, siteID, deviceID int64, stage string) {
	recordEvent(ctx, store, gatewayEvent{
		siteID: siteID, deviceID: &deviceID, occurredAt: time.Now().UTC(),
		eventType: eventDeviceFailed, message: "Device failed", stage: stage,
	})
	fmt.Fprintf(os.Stderr, "WARN: camera failed [device_id=%d]\n", deviceID)
}

func recordEvent(ctx context.Context, store *postgresStore, event gatewayEvent) {
	eventContext := ctx
	cancel := func() {}
	if ctx.Err() != nil {
		eventContext, cancel = context.WithTimeout(context.Background(), shutdownOperationTimeout)
	}
	defer cancel()

	if err := store.RecordEvent(eventContext, event); err != nil {
		if event.deviceID == nil {
			fmt.Fprintf(os.Stderr, "WARN: gateway log write failed [site_id=%d event=%s]\n", event.siteID, event.eventType)
			return
		}
		fmt.Fprintf(
			os.Stderr,
			"WARN: gateway log write failed [site_id=%d device_id=%d event=%s]\n",
			event.siteID,
			*event.deviceID,
			event.eventType,
		)
	}
}

type deviceObserver struct {
	siteID              int64
	deviceID            int64
	store               *postgresStore
	latestObservedAt    time.Time
	lastPersistedSeenAt time.Time
}

func (observer *deviceObserver) connected(ctx context.Context, at time.Time) error {
	at = at.UTC()
	observer.observe(at)
	if err := observer.store.MarkConnected(ctx, observer.siteID, observer.deviceID, at); err != nil {
		return deviceFailure{stage: failureStageDatabase}
	}
	observer.lastPersistedSeenAt = laterTime(observer.lastPersistedSeenAt, at)
	return nil
}

func (observer *deviceObserver) frameObserved(ctx context.Context, at time.Time) error {
	observer.observe(at.UTC())
	if observer.lastPersistedSeenAt.IsZero() ||
		observer.latestObservedAt.Sub(observer.lastPersistedSeenAt) >= frameSeenPersistenceInterval {
		return observer.persistLatestSeen(ctx)
	}
	return nil
}

func (observer *deviceObserver) uploaded(ctx context.Context, capturedAt, completedAt time.Time) error {
	capturedAt = capturedAt.UTC()
	completedAt = completedAt.UTC()
	observer.observe(capturedAt)
	if err := observer.store.MarkUploaded(
		ctx,
		observer.siteID,
		observer.deviceID,
		capturedAt,
		completedAt,
	); err != nil {
		return deviceFailure{stage: failureStageDatabase}
	}
	observer.lastPersistedSeenAt = laterTime(observer.lastPersistedSeenAt, capturedAt)
	return nil
}

func (observer *deviceObserver) flush(ctx context.Context) error {
	if observer.latestObservedAt.IsZero() ||
		!observer.latestObservedAt.After(observer.lastPersistedSeenAt) {
		return nil
	}
	return observer.persistLatestSeen(ctx)
}

func (observer *deviceObserver) observe(at time.Time) {
	if at.After(observer.latestObservedAt) {
		observer.latestObservedAt = at
	}
}

func (observer *deviceObserver) persistLatestSeen(ctx context.Context) error {
	if err := observer.store.AdvanceFrameSeen(
		ctx,
		observer.siteID,
		observer.deviceID,
		observer.latestObservedAt,
	); err != nil {
		return deviceFailure{stage: failureStageDatabase}
	}
	observer.lastPersistedSeenAt = observer.latestObservedAt
	return nil
}

func cameraConfigFromDevice(device deviceRecord) (cameraConfig, error) {
	var source sourceConfig
	if err := json.Unmarshal(device.rtspConfig, &source); err != nil {
		return cameraConfig{}, deviceFailure{stage: failureStageStream}
	}

	var capture captureConfig
	if err := json.Unmarshal(device.captureConfig, &capture); err != nil {
		return cameraConfig{}, deviceFailure{stage: failureStageStream}
	}

	return cameraConfig{
		source:  source,
		capture: capture,
		gcsURI:  device.gcsURI,
	}, nil
}

func laterTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}
