package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/storage"
)

const (
	frameSeenPersistenceInterval = time.Minute
	shutdownOperationTimeout     = 5 * time.Second
)

type cameraRunner func(
	context.Context,
	CameraConfig,
	*storage.Client,
	cameraCallbacks,
	func() time.Time,
) error

type sharedGCSClient struct {
	client    *storage.Client
	closeFunc func() error
}

func (client *sharedGCSClient) Close() error {
	if client == nil || client.closeFunc == nil {
		return nil
	}
	return client.closeFunc()
}

type commissionDependencies struct {
	credentialPath string
	openStore      func(context.Context, string) (gatewayStore, error)
	openGCS        func(context.Context, string) (*sharedGCSClient, error)
	runCamera      cameraRunner
	clock          func() time.Time
	stdout         io.Writer
	stderr         io.Writer
}

type workerResult struct {
	deviceID int64
	err      error
}

type runtimeFactError struct{}

func (runtimeFactError) Error() string {
	return "database runtime fact update failed"
}

// Commission loads one Site from Postgres and runs one independent Phase 2
// camera worker for every valid enabled Device until cancellation or until all
// workers terminate.
func Commission(ctx context.Context, siteID int64) error {
	credentialPath, err := serviceAccountPath()
	if err != nil {
		return err
	}
	return commission(ctx, siteID, commissionDependencies{
		credentialPath: credentialPath,
		openStore:      newPostgresStore,
		openGCS:        newSharedGCSClient,
		runCamera:      runCommissionCamera,
		clock:          time.Now,
		stdout:         os.Stdout,
		stderr:         os.Stderr,
	})
}

func commission(ctx context.Context, siteID int64, dependencies commissionDependencies) (returnErr error) {
	dependencies = normalizedCommissionDependencies(dependencies)
	if siteID <= 0 {
		return errors.New("site_id must be greater than 0")
	}
	if err := requireServiceAccount(dependencies.credentialPath); err != nil {
		return err
	}

	commissioningStartedAt := dependencies.clock().UTC()
	store, err := dependencies.openStore(ctx, dependencies.credentialPath)
	if err != nil {
		return sanitizedCommissionError(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			fmt.Fprintln(dependencies.stderr, "WARN: Cloud SQL shutdown failed")
			returnErr = errors.Join(returnErr, errors.New("Cloud SQL shutdown failed"))
		}
	}()
	databaseConnectedAt := dependencies.clock().UTC()

	site, devices, err := store.LoadSite(ctx, siteID)
	if errors.Is(err, errSiteNotFound) {
		return errors.New("site not found")
	}
	if err != nil {
		return sanitizedCommissionError(err)
	}

	writeEvent(ctx, store, dependencies.stderr, gatewayEvent{
		SiteID: site.ID, OccurredAt: commissioningStartedAt,
		Type: eventGatewayCommissioningStarted, Message: eventMessage(eventGatewayCommissioningStarted),
	})
	writeEvent(ctx, store, dependencies.stderr, gatewayEvent{
		SiteID: site.ID, OccurredAt: databaseConnectedAt,
		Type: eventGatewayDatabaseConnected, Message: eventMessage(eventGatewayDatabaseConnected),
	})
	writeEvent(ctx, store, dependencies.stderr, gatewayEvent{
		SiteID: site.ID, OccurredAt: dependencies.clock().UTC(),
		Type: eventGatewaySiteLoaded, Message: eventMessage(eventGatewaySiteLoaded),
	})

	if site.Status != "enabled" {
		reason := "invalid_site_status"
		if site.Status == "disabled" {
			reason = "site_disabled"
		}
		writeEvent(ctx, store, dependencies.stderr, gatewayEvent{
			SiteID: site.ID, OccurredAt: dependencies.clock().UTC(),
			Type: eventGatewayCommissioningFailed, Message: eventMessage(eventGatewayCommissioningFailed),
			Details: map[string]any{"reason_code": reason, "site_status": site.Status},
		})
		if site.Status == "disabled" {
			return errors.New("site is disabled")
		}
		return errors.New("site status is invalid")
	}
	if len(devices) == 0 {
		writeEvent(ctx, store, dependencies.stderr, gatewayEvent{
			SiteID: site.ID, OccurredAt: dependencies.clock().UTC(),
			Type: eventGatewayCommissioningFailed, Message: eventMessage(eventGatewayCommissioningFailed),
			Details: map[string]any{"reason_code": "site_has_no_devices"},
		})
		return errors.New("site has no Devices")
	}

	configs := make([]CameraConfig, 0, len(devices))
	invalidDevices := 0
	disabledDevices := 0
	for _, device := range devices {
		deviceID := device.ID
		if device.Status == "disabled" {
			disabledDevices++
			writeEvent(ctx, store, dependencies.stderr, gatewayEvent{
				SiteID: site.ID, DeviceID: &deviceID, OccurredAt: dependencies.clock().UTC(),
				Type: eventDeviceWorkerSkipped, Message: eventMessage(eventDeviceWorkerSkipped),
				Details: map[string]any{"reason_code": "device_disabled", "device_status": device.Status},
			})
			continue
		}
		if device.Status != "enabled" {
			invalidDevices++
			writeEvent(ctx, store, dependencies.stderr, gatewayEvent{
				SiteID: site.ID, DeviceID: &deviceID, OccurredAt: dependencies.clock().UTC(),
				Type: eventDeviceConfigurationInvalid, Message: eventMessage(eventDeviceConfigurationInvalid),
				Details: map[string]any{"reason_code": "invalid_device_status", "device_status": device.Status},
			})
			continue
		}

		config, reason, err := cameraConfigFromDevice(site.ID, device)
		if err != nil {
			invalidDevices++
			writeEvent(ctx, store, dependencies.stderr, gatewayEvent{
				SiteID: site.ID, DeviceID: &deviceID, OccurredAt: dependencies.clock().UTC(),
				Type: eventDeviceConfigurationInvalid, Message: eventMessage(eventDeviceConfigurationInvalid),
				Details: map[string]any{"reason_code": reason},
			})
			continue
		}
		configs = append(configs, config)
	}

	if len(configs) == 0 {
		writeEvent(ctx, store, dependencies.stderr, gatewayEvent{
			SiteID: site.ID, OccurredAt: dependencies.clock().UTC(),
			Type: eventGatewayCommissioningFailed, Message: eventMessage(eventGatewayCommissioningFailed),
			Details: map[string]any{
				"reason_code":      "no_usable_devices",
				"valid_devices":    0,
				"invalid_devices":  invalidDevices,
				"disabled_devices": disabledDevices,
			},
		})
		return errors.New("site has no usable Devices")
	}

	gcs, err := dependencies.openGCS(ctx, dependencies.credentialPath)
	if err != nil {
		writeEvent(ctx, store, dependencies.stderr, gatewayEvent{
			SiteID: site.ID, OccurredAt: dependencies.clock().UTC(),
			Type: eventGatewayCommissioningFailed, Message: eventMessage(eventGatewayCommissioningFailed),
			Details: map[string]any{"reason_code": "gcs_client_initialization_failed"},
		})
		return sanitizedCommissionError(err)
	}
	fmt.Fprintf(dependencies.stdout, "Site %d loaded: %d camera worker(s)\n", site.ID, len(configs))
	runErr := runSiteSupervisor(ctx, site, configs, store, gcs.client, dependencies)
	gcsCloseErr := gcs.Close()
	if gcsCloseErr != nil {
		writeEventWithCleanup(store, dependencies.stderr, gatewayEvent{
			SiteID: site.ID, OccurredAt: dependencies.clock().UTC(),
			Type: eventGatewayShutdownFailed, Message: eventMessage(eventGatewayShutdownFailed),
			Details: map[string]any{"reason_code": "gcs_client_close_failed"},
		})
	}
	if ctx.Err() != nil && runErr == nil {
		if gcsCloseErr != nil {
			return errors.New("GCS shutdown failed")
		}
		writeEventWithCleanup(store, dependencies.stderr, gatewayEvent{
			SiteID: site.ID, OccurredAt: dependencies.clock().UTC(),
			Type: eventGatewayShutdownCompleted, Message: eventMessage(eventGatewayShutdownCompleted),
		})
		return nil
	}
	if runErr != nil {
		writeEventWithCleanup(store, dependencies.stderr, gatewayEvent{
			SiteID: site.ID, OccurredAt: dependencies.clock().UTC(),
			Type: eventGatewayCommissioningFailed, Message: eventMessage(eventGatewayCommissioningFailed),
			Details: map[string]any{"reason_code": "all_workers_stopped"},
		})
		if gcsCloseErr != nil {
			return errors.Join(runErr, errors.New("GCS shutdown failed"))
		}
		return runErr
	}
	if gcsCloseErr != nil {
		return errors.New("GCS shutdown failed")
	}
	return nil
}

func normalizedCommissionDependencies(dependencies commissionDependencies) commissionDependencies {
	if dependencies.clock == nil {
		dependencies.clock = time.Now
	}
	if dependencies.stdout == nil {
		dependencies.stdout = io.Discard
	}
	if dependencies.stderr == nil {
		dependencies.stderr = io.Discard
	}
	return dependencies
}

func newSharedGCSClient(ctx context.Context, credentialPath string) (*sharedGCSClient, error) {
	client, err := newStorageClient(ctx, credentialPath)
	if err != nil {
		return nil, err
	}
	return &sharedGCSClient{client: client, closeFunc: client.Close}, nil
}

func runCommissionCamera(
	ctx context.Context,
	config CameraConfig,
	client *storage.Client,
	callbacks cameraCallbacks,
	clock func() time.Time,
) error {
	_, err := connectCamera(ctx, config, client, callbacks, clock)
	return err
}

func runSiteSupervisor(
	ctx context.Context,
	site siteRecord,
	configs []CameraConfig,
	store gatewayStore,
	gcsClient *storage.Client,
	dependencies commissionDependencies,
) error {
	results := make(chan workerResult, len(configs))
	for _, config := range configs {
		config := config
		go func() {
			results <- runDeviceWorker(ctx, site.ID, config, store, gcsClient, dependencies)
		}()
	}

	remaining := len(configs)
	contextDone := ctx.Done()
	shutdownLogged := false
	for remaining > 0 {
		select {
		case <-contextDone:
			contextDone = nil
			shutdownLogged = true
			writeEventWithCleanup(store, dependencies.stderr, gatewayEvent{
				SiteID: site.ID, OccurredAt: dependencies.clock().UTC(),
				Type: eventGatewayShutdownRequested, Message: eventMessage(eventGatewayShutdownRequested),
				Details: map[string]any{"worker_count": len(configs)},
			})
		case <-results:
			remaining--
		}
	}

	if ctx.Err() != nil {
		if !shutdownLogged {
			writeEventWithCleanup(store, dependencies.stderr, gatewayEvent{
				SiteID: site.ID, OccurredAt: dependencies.clock().UTC(),
				Type: eventGatewayShutdownRequested, Message: eventMessage(eventGatewayShutdownRequested),
				Details: map[string]any{"worker_count": len(configs)},
			})
		}
		return nil
	}
	return errors.New("all camera workers stopped")
}

func runDeviceWorker(
	ctx context.Context,
	siteID int64,
	config CameraConfig,
	store gatewayStore,
	gcsClient *storage.Client,
	dependencies commissionDependencies,
) workerResult {
	deviceID := config.DeviceID
	observer := &deviceObserver{
		siteID: siteID, deviceID: deviceID, store: store,
		clock: dependencies.clock, stderr: dependencies.stderr,
	}
	writeEvent(ctx, store, dependencies.stderr, gatewayEvent{
		SiteID: siteID, DeviceID: &deviceID, OccurredAt: dependencies.clock().UTC(),
		Type: eventDeviceWorkerStarted, Message: eventMessage(eventDeviceWorkerStarted),
	})
	writeEvent(ctx, store, dependencies.stderr, gatewayEvent{
		SiteID: siteID, DeviceID: &deviceID, OccurredAt: dependencies.clock().UTC(),
		Type: eventDeviceRTSPConnectionAttempt, Message: eventMessage(eventDeviceRTSPConnectionAttempt),
	})

	runErr := dependencies.runCamera(ctx, config, gcsClient, observer.callbacks(), dependencies.clock)
	flushCtx := ctx
	flushCancel := func() {}
	if ctx.Err() != nil {
		flushCtx, flushCancel = context.WithTimeout(context.Background(), shutdownOperationTimeout)
	}
	flushErr := observer.flush(flushCtx)
	flushCancel()

	if runErr != nil && ctx.Err() == nil {
		var factError runtimeFactError
		if !errors.As(runErr, &factError) {
			eventType, reason, details := classifyCameraFailure(runErr)
			details["reason_code"] = reason
			writeEvent(ctx, store, dependencies.stderr, gatewayEvent{
				SiteID: siteID, DeviceID: &deviceID, OccurredAt: dependencies.clock().UTC(),
				Type: eventType, Message: eventMessage(eventType), Details: details,
			})
		}
	}
	stopReason := "cancelled"
	if ctx.Err() == nil {
		stopReason = "worker_terminated"
	}
	writeEventWithContext(ctx, store, dependencies.stderr, gatewayEvent{
		SiteID: siteID, DeviceID: &deviceID, OccurredAt: dependencies.clock().UTC(),
		Type: eventDeviceWorkerStopped, Message: eventMessage(eventDeviceWorkerStopped),
		Details: map[string]any{"reason_code": stopReason},
	})

	if ctx.Err() != nil {
		if flushErr != nil {
			return workerResult{deviceID: deviceID, err: errors.New("camera worker shutdown failed")}
		}
		return workerResult{deviceID: deviceID}
	}
	if runErr == nil {
		runErr = errors.New("camera worker stopped unexpectedly")
	}
	if flushErr != nil {
		return workerResult{deviceID: deviceID, err: errors.New("camera worker failed")}
	}
	return workerResult{deviceID: deviceID, err: sanitizedCameraError(runErr)}
}

type deviceObserver struct {
	siteID              int64
	deviceID            int64
	store               gatewayStore
	clock               func() time.Time
	stderr              io.Writer
	latestObservedAt    time.Time
	lastPersistedSeenAt time.Time
}

func (observer *deviceObserver) callbacks() cameraCallbacks {
	return cameraCallbacks{
		ffmpegStarted: observer.ffmpegStarted,
		ffmpegExited:  observer.ffmpegExited,
		connected:     observer.connected,
		frameObserved: observer.frameObserved,
		uploaded:      observer.uploaded,
	}
}

func (observer *deviceObserver) ffmpegStarted(ctx context.Context, at time.Time) {
	deviceID := observer.deviceID
	writeEvent(ctx, observer.store, observer.stderr, gatewayEvent{
		SiteID: observer.siteID, DeviceID: &deviceID, OccurredAt: at,
		Type: eventDeviceFFmpegStarted, Message: eventMessage(eventDeviceFFmpegStarted),
	})
}

func (observer *deviceObserver) ffmpegExited(ctx context.Context, at time.Time, exitCode *int) {
	details := map[string]any{}
	if exitCode != nil {
		details["ffmpeg_exit_code"] = *exitCode
	}
	deviceID := observer.deviceID
	writeEventWithContext(ctx, observer.store, observer.stderr, gatewayEvent{
		SiteID: observer.siteID, DeviceID: &deviceID, OccurredAt: at,
		Type: eventDeviceFFmpegExited, Message: eventMessage(eventDeviceFFmpegExited), Details: details,
	})
}

func (observer *deviceObserver) connected(ctx context.Context, at time.Time) error {
	at = at.UTC()
	observer.observe(at)
	if err := observer.store.MarkConnected(ctx, observer.siteID, observer.deviceID, at); err != nil {
		return observer.databaseFailure(ctx, "last_connected_update_failed")
	}
	observer.lastPersistedSeenAt = laterTime(observer.lastPersistedSeenAt, at)
	deviceID := observer.deviceID
	writeEvent(ctx, observer.store, observer.stderr, gatewayEvent{
		SiteID: observer.siteID, DeviceID: &deviceID, OccurredAt: at,
		Type: eventDeviceRTSPConnected, Message: eventMessage(eventDeviceRTSPConnected),
	})
	return nil
}

func (observer *deviceObserver) frameObserved(ctx context.Context, at time.Time) error {
	observer.observe(at.UTC())
	if observer.lastPersistedSeenAt.IsZero() ||
		observer.latestObservedAt.Sub(observer.lastPersistedSeenAt) >= frameSeenPersistenceInterval {
		if err := observer.persistLatestSeen(ctx); err != nil {
			return err
		}
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
		return observer.databaseFailure(ctx, "upload_fact_transaction_failed")
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
		return observer.databaseFailure(ctx, "last_frame_seen_update_failed")
	}
	observer.lastPersistedSeenAt = observer.latestObservedAt
	return nil
}

func (observer *deviceObserver) databaseFailure(ctx context.Context, reason string) error {
	deviceID := observer.deviceID
	writeEventWithContext(ctx, observer.store, observer.stderr, gatewayEvent{
		SiteID: observer.siteID, DeviceID: &deviceID, OccurredAt: observer.clock().UTC(),
		Type: eventDeviceDatabaseWriteFailed, Message: eventMessage(eventDeviceDatabaseWriteFailed),
		Details: map[string]any{"reason_code": reason},
	})
	return runtimeFactError{}
}

func cameraConfigFromDevice(siteID int64, device deviceRecord) (CameraConfig, string, error) {
	if device.SiteID != siteID || device.ID <= 0 {
		return CameraConfig{}, "invalid_device_identity", errors.New("Device configuration invalid")
	}

	var source SourceConfig
	if err := decodeConfigObject(device.RTSPConfig, &source); err != nil {
		if isNullJSON(device.RTSPConfig) {
			return CameraConfig{}, "null_rtsp_config", errors.New("Device configuration invalid")
		}
		return CameraConfig{}, "malformed_rtsp_config", errors.New("Device configuration invalid")
	}
	var capture CaptureConfig
	if err := decodeConfigObject(device.CaptureConfig, &capture); err != nil {
		return CameraConfig{}, "malformed_capture_config", errors.New("Device configuration invalid")
	}

	config := CameraConfig{
		Version: 2, DeviceID: device.ID, Name: device.Name,
		Source: source, Capture: capture,
		Destination: DestinationConfig{GCSURI: device.GCSURI},
	}
	if err := validateConfig(config); err != nil {
		switch {
		case config.Capture.FPS != 3:
			return CameraConfig{}, "unsupported_fps", errors.New("Device configuration invalid")
		case math.IsNaN(config.Capture.ChangeThresholdPercent),
			math.IsInf(config.Capture.ChangeThresholdPercent, 0),
			config.Capture.ChangeThresholdPercent <= 0,
			config.Capture.ChangeThresholdPercent > 100:
			return CameraConfig{}, "invalid_change_threshold", errors.New("Device configuration invalid")
		case strings.TrimSpace(config.Destination.GCSURI) == "":
			return CameraConfig{}, "invalid_gcs_uri", errors.New("Device configuration invalid")
		case strings.TrimSpace(config.Source.URI) == "":
			return CameraConfig{}, "invalid_rtsp_config", errors.New("Device configuration invalid")
		default:
			if _, parseErr := parseGCSURI(config.Destination.GCSURI); parseErr != nil {
				return CameraConfig{}, "invalid_gcs_uri", errors.New("Device configuration invalid")
			}
			return CameraConfig{}, "invalid_device_configuration", errors.New("Device configuration invalid")
		}
	}
	return config, "", nil
}

func decodeConfigObject(data []byte, destination any) error {
	if isNullJSON(data) {
		return errors.New("configuration is null")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return errors.New("configuration is malformed")
	}
	if decoder.More() {
		return errors.New("configuration is malformed")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("configuration is malformed")
	}
	return nil
}

func isNullJSON(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func laterTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

func classifyCameraFailure(err error) (string, string, map[string]any) {
	details := map[string]any{}
	var incomplete incompleteFrameError
	if errors.As(err, &incomplete) {
		details["received_bytes"] = incomplete.received
		return eventDeviceFramePipelineFailed, "incomplete_frame", details
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "RTSP authentication failed"):
		return eventDeviceRTSPFailed, "authentication_failed", details
	case strings.Contains(message, "RTSP connection failed"):
		return eventDeviceRTSPFailed, "connection_failed", details
	case strings.Contains(message, "GCS authentication failed"):
		return eventDeviceGCSUploadFailed, "gcs_authentication_failed", details
	case strings.Contains(message, "GCS upload failed"):
		return eventDeviceGCSUploadFailed, "gcs_upload_failed", details
	case strings.Contains(message, "database runtime fact"):
		return eventDeviceDatabaseWriteFailed, "runtime_fact_update_failed", details
	case strings.Contains(message, "ffmpeg not found"), strings.Contains(message, "FFmpeg could not start"):
		return eventDeviceFramePipelineFailed, "ffmpeg_start_failed", details
	case strings.Contains(message, "stream ended"):
		return eventDeviceFramePipelineFailed, "stream_ended", details
	default:
		return eventDeviceFramePipelineFailed, "frame_pipeline_failed", details
	}
}

func sanitizedCameraError(_ error) error {
	return errors.New("camera worker failed")
}

func sanitizedCommissionError(err error) error {
	message := err.Error()
	switch {
	case strings.HasPrefix(message, "sa.json"):
		return errors.New(message)
	case strings.HasPrefix(message, "GCS"):
		return errors.New(message)
	case strings.HasPrefix(message, "Cloud SQL"):
		return errors.New(message)
	case strings.HasPrefix(message, "database"):
		return errors.New(message)
	default:
		return errors.New("Gateway commissioning failed")
	}
}

func writeEvent(ctx context.Context, store gatewayStore, stderr io.Writer, event gatewayEvent) {
	if err := store.RecordEvent(ctx, event); err != nil {
		writeLogFailureDiagnostic(stderr, event)
	}
}

func writeEventWithContext(ctx context.Context, store gatewayStore, stderr io.Writer, event gatewayEvent) {
	if ctx.Err() == nil {
		writeEvent(ctx, store, stderr, event)
		return
	}
	writeEventWithCleanup(store, stderr, event)
}

func writeEventWithCleanup(store gatewayStore, stderr io.Writer, event gatewayEvent) {
	cleanupContext, cancel := context.WithTimeout(context.Background(), shutdownOperationTimeout)
	defer cancel()
	writeEvent(cleanupContext, store, stderr, event)
}

func writeLogFailureDiagnostic(stderr io.Writer, event gatewayEvent) {
	if event.DeviceID == nil {
		fmt.Fprintf(stderr, "WARN: gateway log write failed (site_id=%d event_type=%s)\n", event.SiteID, event.Type)
		return
	}
	fmt.Fprintf(
		stderr,
		"WARN: gateway log write failed (site_id=%d device_id=%d event_type=%s)\n",
		event.SiteID,
		*event.DeviceID,
		event.Type,
	)
}

func eventMessage(eventType string) string {
	switch eventType {
	case eventGatewayCommissioningStarted:
		return "Gateway commissioning started"
	case eventGatewayDatabaseConnected:
		return "Cloud SQL connection established"
	case eventGatewaySiteLoaded:
		return "Site configuration loaded"
	case eventGatewayCommissioningFailed:
		return "Gateway commissioning failed"
	case eventGatewayShutdownRequested:
		return "Gateway shutdown requested"
	case eventGatewayShutdownCompleted:
		return "Gateway shutdown completed"
	case eventGatewayShutdownFailed:
		return "Gateway shutdown failed"
	case eventDeviceConfigurationInvalid:
		return "Device configuration is invalid"
	case eventDeviceWorkerSkipped:
		return "Device worker skipped"
	case eventDeviceWorkerStarted:
		return "Device worker started"
	case eventDeviceRTSPConnectionAttempt:
		return "RTSP connection attempt started"
	case eventDeviceRTSPConnected:
		return "RTSP stream became operational"
	case eventDeviceRTSPFailed:
		return "RTSP stream failed"
	case eventDeviceFFmpegStarted:
		return "FFmpeg process started"
	case eventDeviceFFmpegExited:
		return "FFmpeg process exited"
	case eventDeviceFramePipelineFailed:
		return "Frame pipeline failed"
	case eventDeviceGCSUploadFailed:
		return "GCS upload failed"
	case eventDeviceDatabaseWriteFailed:
		return "Required database runtime write failed"
	case eventDeviceWorkerStopped:
		return "Device worker stopped"
	default:
		return "Gateway event"
	}
}
