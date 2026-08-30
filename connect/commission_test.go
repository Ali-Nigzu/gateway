package connect

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
)

type connectedFact struct {
	siteID, deviceID int64
	at               time.Time
}

type uploadedFact struct {
	siteID, deviceID    int64
	captured, completed time.Time
}

type fakeGatewayStore struct {
	mu sync.Mutex

	site    siteRecord
	devices []deviceRecord
	loadErr error

	recordErr    error
	connectedErr error
	frameSeenErr error
	uploadErr    error
	closeErr     error

	events     []gatewayEvent
	connected  []connectedFact
	frameSeen  []connectedFact
	uploaded   []uploadedFact
	closeCalls int
}

func (store *fakeGatewayStore) LoadSite(context.Context, int64) (siteRecord, []deviceRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.site, append([]deviceRecord(nil), store.devices...), store.loadErr
}

func (store *fakeGatewayStore) RecordEvent(_ context.Context, event gatewayEvent) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	copyEvent := event
	if event.DeviceID != nil {
		deviceID := *event.DeviceID
		copyEvent.DeviceID = &deviceID
	}
	copyEvent.Details = copyDetails(event.Details)
	store.events = append(store.events, copyEvent)
	return store.recordErr
}

func (store *fakeGatewayStore) MarkConnected(_ context.Context, siteID, deviceID int64, at time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.connected = append(store.connected, connectedFact{siteID: siteID, deviceID: deviceID, at: at})
	return store.connectedErr
}

func (store *fakeGatewayStore) AdvanceFrameSeen(_ context.Context, siteID, deviceID int64, at time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.frameSeen = append(store.frameSeen, connectedFact{siteID: siteID, deviceID: deviceID, at: at})
	return store.frameSeenErr
}

func (store *fakeGatewayStore) MarkUploaded(_ context.Context, siteID, deviceID int64, captured, completed time.Time) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.uploaded = append(store.uploaded, uploadedFact{
		siteID: siteID, deviceID: deviceID, captured: captured, completed: completed,
	})
	return store.uploadErr
}

func (store *fakeGatewayStore) Close() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.closeCalls++
	return store.closeErr
}

func (store *fakeGatewayStore) snapshot() (events []gatewayEvent, connected, seen []connectedFact, uploaded []uploadedFact, closes int) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]gatewayEvent(nil), store.events...),
		append([]connectedFact(nil), store.connected...),
		append([]connectedFact(nil), store.frameSeen...),
		append([]uploadedFact(nil), store.uploaded...),
		store.closeCalls
}

type synchronizedBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *synchronizedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(data)
}

func (buffer *synchronizedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func TestCameraConfigFromDeviceUsesDatabaseIdentityAndPerDeviceDestination(t *testing.T) {
	device := validDevice(37)
	config, reason, err := cameraConfigFromDevice(1, device)
	if err != nil {
		t.Fatalf("cameraConfigFromDevice() error = %v, reason = %q", err, reason)
	}
	if config.DeviceID != 37 || config.Name != device.Name {
		t.Fatalf("database identity was not preserved: %#v", config)
	}
	if config.Source.URI != "rtsp://camera-37.example/live" || config.Source.Username != "user-37" {
		t.Fatalf("RTSP config was not mapped: %#v", config.Source)
	}
	if config.Capture.FPS != 3 || config.Capture.ChangeThresholdPercent != 0.5 {
		t.Fatalf("capture config was not mapped: %#v", config.Capture)
	}
	if config.Destination.GCSURI != "gs://camera-bucket/site-1/device-37/" {
		t.Fatalf("GCS destination = %q", config.Destination.GCSURI)
	}

	second, _, err := cameraConfigFromDevice(1, validDevice(811))
	if err != nil {
		t.Fatalf("second Device mapping failed: %v", err)
	}
	if second.DeviceID != 811 || second.Destination.GCSURI == config.Destination.GCSURI {
		t.Fatalf("Device destinations crossed: first=%#v second=%#v", config, second)
	}
}

func TestCameraConfigFromDeviceRejectsMalformedDatabaseConfiguration(t *testing.T) {
	tests := []struct {
		name       string
		mutate     func(*deviceRecord)
		wantReason string
	}{
		{name: "identity", mutate: func(device *deviceRecord) { device.SiteID = 2 }, wantReason: "invalid_device_identity"},
		{name: "malformed RTSP", mutate: func(device *deviceRecord) { device.RTSPConfig = []byte(`{"uri":`) }, wantReason: "malformed_rtsp_config"},
		{name: "NULL RTSP", mutate: func(device *deviceRecord) { device.RTSPConfig = []byte(`null`) }, wantReason: "null_rtsp_config"},
		{name: "unknown RTSP field", mutate: func(device *deviceRecord) { device.RTSPConfig = []byte(`{"uri":"rtsp://camera/live","extra":true}`) }, wantReason: "malformed_rtsp_config"},
		{name: "malformed capture", mutate: func(device *deviceRecord) { device.CaptureConfig = []byte(`{"fps":`) }, wantReason: "malformed_capture_config"},
		{name: "unsupported FPS", mutate: func(device *deviceRecord) { device.CaptureConfig = []byte(`{"fps":2,"change_threshold_percent":0.5}`) }, wantReason: "unsupported_fps"},
		{name: "invalid threshold", mutate: func(device *deviceRecord) { device.CaptureConfig = []byte(`{"fps":3,"change_threshold_percent":0}`) }, wantReason: "invalid_change_threshold"},
		{name: "invalid GCS URI", mutate: func(device *deviceRecord) { device.GCSURI = "https://bucket/device" }, wantReason: "invalid_gcs_uri"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			device := validDevice(37)
			test.mutate(&device)
			_, reason, err := cameraConfigFromDevice(1, device)
			if err == nil || reason != test.wantReason {
				t.Fatalf("error = %v, reason = %q, want %q", err, reason, test.wantReason)
			}
		})
	}
}

func TestCommissionGlobalConfigurationFailures(t *testing.T) {
	tests := []struct {
		name     string
		site     siteRecord
		devices  []deviceRecord
		loadErr  error
		wantText string
	}{
		{name: "site not found", loadErr: errSiteNotFound, wantText: "site not found"},
		{name: "disabled Site", site: siteRecord{ID: 1, Status: "disabled"}, devices: []deviceRecord{validDevice(1)}, wantText: "site is disabled"},
		{name: "invalid Site status", site: siteRecord{ID: 1, Status: "offline"}, devices: []deviceRecord{validDevice(1)}, wantText: "site status is invalid"},
		{name: "zero Devices", site: siteRecord{ID: 1, Status: "enabled"}, wantText: "site has no Devices"},
		{name: "no usable Devices", site: siteRecord{ID: 1, Status: "enabled"}, devices: []deviceRecord{disabledDevice(1), invalidDevice(2)}, wantText: "site has no usable Devices"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &fakeGatewayStore{site: test.site, devices: test.devices, loadErr: test.loadErr}
			dependencies, _, _ := commissionTestDependencies(t, store, func(context.Context, CameraConfig, *storage.Client, cameraCallbacks, func() time.Time) error {
				t.Fatal("camera worker started for invalid commissioning input")
				return nil
			})
			err := commission(context.Background(), 1, dependencies)
			if err == nil || err.Error() != test.wantText {
				t.Fatalf("commission() error = %v, want %q", err, test.wantText)
			}
		})
	}
}

func TestCommissionRejectsInvalidSiteIDAndMissingServiceAccount(t *testing.T) {
	store := &fakeGatewayStore{}
	dependencies, _, _ := commissionTestDependencies(t, store, nil)
	if err := commission(context.Background(), 0, dependencies); err == nil || err.Error() != "site_id must be greater than 0" {
		t.Fatalf("invalid site ID error = %v", err)
	}

	dependencies.credentialPath = filepath.Join(t.TempDir(), "sa.json")
	if err := commission(context.Background(), 1, dependencies); err == nil || err.Error() != "sa.json not found" {
		t.Fatalf("missing sa.json error = %v", err)
	}
}

func TestCommissionStartsExactlyOneWorkerPerValidEnabledDevice(t *testing.T) {
	for _, count := range []int{1, 2, 5, 20} {
		t.Run(fmt.Sprintf("%d Devices", count), func(t *testing.T) {
			devices := make([]deviceRecord, 0, count)
			wantIDs := make([]int64, 0, count)
			for index := 0; index < count; index++ {
				id := int64(901 + index*17)
				devices = append(devices, validDevice(id))
				wantIDs = append(wantIDs, id)
			}
			store := &fakeGatewayStore{site: siteRecord{ID: 1, Status: "enabled"}, devices: devices}
			started := make(chan int64, count)
			var finished atomic.Int32
			runner := func(ctx context.Context, config CameraConfig, _ *storage.Client, _ cameraCallbacks, _ func() time.Time) error {
				started <- config.DeviceID
				<-ctx.Done()
				finished.Add(1)
				return ctx.Err()
			}
			dependencies, gcsCloses, _ := commissionTestDependencies(t, store, runner)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- commission(ctx, 1, dependencies) }()

			gotIDs := make([]int64, 0, count)
			for len(gotIDs) < count {
				select {
				case id := <-started:
					gotIDs = append(gotIDs, id)
				case <-time.After(5 * time.Second):
					t.Fatalf("only %d/%d workers started", len(gotIDs), count)
				}
			}
			sort.Slice(gotIDs, func(left, right int) bool { return gotIDs[left] < gotIDs[right] })
			sort.Slice(wantIDs, func(left, right int) bool { return wantIDs[left] < wantIDs[right] })
			if fmt.Sprint(gotIDs) != fmt.Sprint(wantIDs) {
				t.Fatalf("worker Device IDs = %v, want %v", gotIDs, wantIDs)
			}

			cancel()
			if err := waitForCommission(t, done); err != nil {
				t.Fatalf("cancelled commission() error = %v", err)
			}
			if finished.Load() != int32(count) {
				t.Fatalf("finished workers = %d, want %d", finished.Load(), count)
			}
			_, _, _, _, dbCloses := store.snapshot()
			if dbCloses != 1 || gcsCloses.Load() != 1 {
				t.Fatalf("resource closes: DB=%d GCS=%d", dbCloses, gcsCloses.Load())
			}
		})
	}
}

func TestCommissionRunsValidSiblingsWhenOtherDevicesAreDisabledOrInvalid(t *testing.T) {
	store := &fakeGatewayStore{
		site: siteRecord{ID: 1, Status: "enabled"},
		devices: []deviceRecord{
			validDevice(37), disabledDevice(52), invalidDevice(91), validDevice(811),
		},
	}
	started := make(chan int64, 2)
	runner := func(ctx context.Context, config CameraConfig, _ *storage.Client, _ cameraCallbacks, _ func() time.Time) error {
		started <- config.DeviceID
		<-ctx.Done()
		return ctx.Err()
	}
	dependencies, _, _ := commissionTestDependencies(t, store, runner)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- commission(ctx, 1, dependencies) }()

	got := []int64{<-started, <-started}
	sort.Slice(got, func(left, right int) bool { return got[left] < got[right] })
	if fmt.Sprint(got) != fmt.Sprint([]int64{37, 811}) {
		t.Fatalf("started Device IDs = %v", got)
	}
	cancel()
	if err := waitForCommission(t, done); err != nil {
		t.Fatalf("commission() error = %v", err)
	}

	events, _, _, _, _ := store.snapshot()
	assertDeviceEvent(t, events, 52, eventDeviceWorkerSkipped)
	assertDeviceEvent(t, events, 91, eventDeviceConfigurationInvalid)
}

func TestWorkerFailuresDoNotCancelHealthySiblings(t *testing.T) {
	for _, failedCount := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d failures", failedCount), func(t *testing.T) {
			devices := []deviceRecord{validDevice(1), validDevice(2), validDevice(3), validDevice(4)}
			store := &fakeGatewayStore{site: siteRecord{ID: 1, Status: "enabled"}, devices: devices}
			healthyStarted := make(chan struct{}, 1)
			var mu sync.Mutex
			calls := map[int64]int{}
			runner := func(ctx context.Context, config CameraConfig, _ *storage.Client, _ cameraCallbacks, _ func() time.Time) error {
				mu.Lock()
				calls[config.DeviceID]++
				mu.Unlock()
				if config.DeviceID <= int64(failedCount) {
					return errors.New("camera failed")
				}
				if config.DeviceID == 4 {
					healthyStarted <- struct{}{}
				}
				<-ctx.Done()
				return ctx.Err()
			}
			dependencies, _, _ := commissionTestDependencies(t, store, runner)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- commission(ctx, 1, dependencies) }()

			select {
			case <-healthyStarted:
			case <-time.After(5 * time.Second):
				t.Fatal("healthy sibling did not start")
			}
			select {
			case err := <-done:
				t.Fatalf("one Device failure stopped Site runtime: %v", err)
			case <-time.After(100 * time.Millisecond):
			}

			cancel()
			if err := waitForCommission(t, done); err != nil {
				t.Fatalf("cancelled commission() error = %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, device := range devices {
				if calls[device.ID] != 1 {
					t.Fatalf("Device %d worker call count = %d, want exactly 1", device.ID, calls[device.ID])
				}
			}
		})
	}
}

func TestAllWorkerExitReturnsWithoutRestart(t *testing.T) {
	devices := []deviceRecord{validDevice(3), validDevice(19), validDevice(204)}
	store := &fakeGatewayStore{site: siteRecord{ID: 1, Status: "enabled"}, devices: devices}
	var mu sync.Mutex
	calls := map[int64]int{}
	runner := func(_ context.Context, config CameraConfig, _ *storage.Client, _ cameraCallbacks, _ func() time.Time) error {
		mu.Lock()
		calls[config.DeviceID]++
		mu.Unlock()
		return errors.New("stream ended")
	}
	dependencies, _, _ := commissionTestDependencies(t, store, runner)
	err := commission(context.Background(), 1, dependencies)
	if err == nil || err.Error() != "all camera workers stopped" {
		t.Fatalf("commission() error = %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, device := range devices {
		if calls[device.ID] != 1 {
			t.Fatalf("Device %d restarted: call count = %d", device.ID, calls[device.ID])
		}
	}
}

func TestStandaloneGatewayLogFailureDoesNotKillHealthyWorker(t *testing.T) {
	secret := "raw-database-secret"
	store := &fakeGatewayStore{
		site: siteRecord{ID: 1, Status: "enabled"}, devices: []deviceRecord{validDevice(1)},
		recordErr: errors.New(secret),
	}
	started := make(chan struct{})
	runner := func(ctx context.Context, _ CameraConfig, _ *storage.Client, _ cameraCallbacks, _ func() time.Time) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	dependencies, _, stderr := commissionTestDependencies(t, store, runner)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- commission(ctx, 1, dependencies) }()
	<-started
	select {
	case err := <-done:
		t.Fatalf("log INSERT failure killed healthy worker: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	if err := waitForCommission(t, done); err != nil {
		t.Fatalf("commission() error = %v", err)
	}
	if output := stderr.String(); !strings.Contains(output, "WARN: gateway log write failed") || strings.Contains(output, secret) {
		t.Fatalf("stderr was not sanitized: %q", output)
	}
}

func TestGCSShutdownFailureIsReportedAndNotMarkedComplete(t *testing.T) {
	store := &fakeGatewayStore{site: siteRecord{ID: 1, Status: "enabled"}, devices: []deviceRecord{validDevice(1)}}
	runner := func(ctx context.Context, _ CameraConfig, _ *storage.Client, _ cameraCallbacks, _ func() time.Time) error {
		<-ctx.Done()
		return ctx.Err()
	}
	dependencies, _, _ := commissionTestDependencies(t, store, runner)
	dependencies.openGCS = func(context.Context, string) (*sharedGCSClient, error) {
		return &sharedGCSClient{closeFunc: func() error { return errors.New("raw close failure") }}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := commission(ctx, 1, dependencies)
	if err == nil || err.Error() != "GCS shutdown failed" {
		t.Fatalf("commission() error = %v", err)
	}
	events, _, _, _, _ := store.snapshot()
	foundFailed := false
	foundCompleted := false
	for _, event := range events {
		foundFailed = foundFailed || event.Type == eventGatewayShutdownFailed
		foundCompleted = foundCompleted || event.Type == eventGatewayShutdownCompleted
	}
	if !foundFailed || foundCompleted {
		t.Fatalf("shutdown events = %#v", events)
	}
}

func TestSecretsDoNotReachCommissionOutputsErrorsOrEvents(t *testing.T) {
	secret := "THIS-IS-A-FAKE-RTSP-PASSWORD"
	device := validDevice(44)
	device.RTSPConfig = []byte(fmt.Sprintf(`{"uri":"rtsp://camera.example/live","username":"user","password":%q}`, secret))
	store := &fakeGatewayStore{site: siteRecord{ID: 1, Status: "enabled"}, devices: []deviceRecord{device}}
	runner := func(context.Context, CameraConfig, *storage.Client, cameraCallbacks, func() time.Time) error {
		return errors.New("failure includes " + secret)
	}
	dependencies, _, stderr := commissionTestDependencies(t, store, runner)
	err := commission(context.Background(), 1, dependencies)
	events, _, _, _, _ := store.snapshot()
	eventData, marshalErr := json.Marshal(events)
	if marshalErr != nil {
		t.Fatalf("marshal events: %v", marshalErr)
	}
	allVisible := fmt.Sprintf("%v\n%s\n%s\n%s", err, dependencies.stdout.(*synchronizedBuffer).String(), stderr.String(), eventData)
	if strings.Contains(allVisible, secret) {
		t.Fatalf("secret leaked into an observable surface: %s", allVisible)
	}
}

func TestDeviceObserverPersistsActualObservationsWithoutPerFrameWrites(t *testing.T) {
	base := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	store := &fakeGatewayStore{}
	observer := &deviceObserver{siteID: 1, deviceID: 37, store: store, clock: func() time.Time { return base }, stderr: &synchronizedBuffer{}}
	callbacks := observer.callbacks()

	if err := callbacks.connected(context.Background(), base); err != nil {
		t.Fatalf("connected() error = %v", err)
	}
	for candidate := 1; candidate <= 360; candidate++ {
		observedAt := base.Add(time.Duration(candidate) * time.Second / 3)
		if err := callbacks.frameObserved(context.Background(), observedAt); err != nil {
			t.Fatalf("frameObserved(%d) error = %v", candidate, err)
		}
	}
	_, connected, seen, uploaded, _ := store.snapshot()
	if len(connected) != 1 || !connected[0].at.Equal(base) {
		t.Fatalf("connected facts = %#v", connected)
	}
	wantSeen := []time.Time{base.Add(time.Minute), base.Add(2 * time.Minute)}
	if len(seen) != len(wantSeen) {
		t.Fatalf("frame-seen writes = %d for 360 candidates, want %d", len(seen), len(wantSeen))
	}
	for index, want := range wantSeen {
		if !seen[index].at.Equal(want) {
			t.Fatalf("frame-seen[%d] = %v, want actual observation %v", index, seen[index].at, want)
		}
	}
	if len(uploaded) != 0 {
		t.Fatalf("frame observations wrote upload facts: %#v", uploaded)
	}
	events, _, _, _, _ := store.snapshot()
	if len(events) != 1 || events[0].Type != eventDeviceRTSPConnected {
		t.Fatalf("normal frame candidates created gateway-log spam: %#v", events)
	}

	captured := base.Add(121 * time.Second)
	completed := base.Add(122 * time.Second)
	if err := callbacks.uploaded(context.Background(), captured, completed); err != nil {
		t.Fatalf("uploaded() error = %v", err)
	}
	if err := callbacks.frameObserved(context.Background(), base.Add(130*time.Second)); err != nil {
		t.Fatalf("later frame error = %v", err)
	}
	if err := observer.flush(context.Background()); err != nil {
		t.Fatalf("flush() error = %v", err)
	}
	if err := observer.flush(context.Background()); err != nil {
		t.Fatalf("second flush() error = %v", err)
	}
	_, _, seen, uploaded, _ = store.snapshot()
	if len(uploaded) != 1 || !uploaded[0].captured.Equal(captured) || !uploaded[0].completed.Equal(completed) {
		t.Fatalf("upload facts = %#v", uploaded)
	}
	if len(seen) != 3 || !seen[2].at.Equal(base.Add(130*time.Second)) {
		t.Fatalf("shutdown flush did not write the newest actual observation: %#v", seen)
	}
	events, _, _, _, _ = store.snapshot()
	if len(events) != 1 {
		t.Fatalf("successful upload created gateway-log spam: %#v", events)
	}
}

func TestConnectionFactIsWrittenOnlyAtOperationalStreamCallback(t *testing.T) {
	at := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	store := &fakeGatewayStore{}
	dependencies := commissionDependencies{
		runCamera: func(context.Context, CameraConfig, *storage.Client, cameraCallbacks, func() time.Time) error {
			return errors.New("RTSP connection failed")
		},
		clock:  func() time.Time { return at },
		stderr: &synchronizedBuffer{},
	}
	runDeviceWorker(context.Background(), 1, representativeConfig(), store, nil, dependencies)
	_, connected, _, _, _ := store.snapshot()
	if len(connected) != 0 {
		t.Fatalf("RTSP attempt incorrectly wrote last_connected_at: %#v", connected)
	}

	store = &fakeGatewayStore{}
	dependencies.runCamera = func(ctx context.Context, _ CameraConfig, _ *storage.Client, callbacks cameraCallbacks, _ func() time.Time) error {
		if err := callbacks.connected(ctx, at); err != nil {
			return err
		}
		return errors.New("stream ended")
	}
	runDeviceWorker(context.Background(), 1, representativeConfig(), store, nil, dependencies)
	_, connected, _, _, _ = store.snapshot()
	if len(connected) != 1 || !connected[0].at.Equal(at) {
		t.Fatalf("operational stream boundary did not write last_connected_at: %#v", connected)
	}
}

func TestRequiredDatabaseFactFailureStopsOnlyThroughCallback(t *testing.T) {
	store := &fakeGatewayStore{uploadErr: errors.New("secret database failure")}
	stderr := &synchronizedBuffer{}
	observer := &deviceObserver{siteID: 1, deviceID: 91, store: store, clock: time.Now, stderr: stderr}
	err := observer.uploaded(context.Background(), time.Now(), time.Now())
	if err == nil || err.Error() != "database runtime fact update failed" {
		t.Fatalf("uploaded() error = %v", err)
	}
	events, _, _, _, _ := store.snapshot()
	assertDeviceEvent(t, events, 91, eventDeviceDatabaseWriteFailed)
	encoded, _ := json.Marshal(events)
	if strings.Contains(string(encoded), "secret database failure") || strings.Contains(stderr.String(), "secret database failure") {
		t.Fatal("required DB failure leaked its raw error")
	}
}

func commissionTestDependencies(t *testing.T, store gatewayStore, runner cameraRunner) (commissionDependencies, *atomic.Int32, *synchronizedBuffer) {
	t.Helper()
	credentialPath := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(credentialPath, []byte(`{"client_email":"gateway@example.iam.gserviceaccount.com"}`), 0o600); err != nil {
		t.Fatalf("write service account fixture: %v", err)
	}
	if runner == nil {
		runner = func(context.Context, CameraConfig, *storage.Client, cameraCallbacks, func() time.Time) error {
			return errors.New("camera worker should not start")
		}
	}
	var gcsCloses atomic.Int32
	stdout := &synchronizedBuffer{}
	stderr := &synchronizedBuffer{}
	return commissionDependencies{
		credentialPath: credentialPath,
		openStore: func(context.Context, string) (gatewayStore, error) {
			return store, nil
		},
		openGCS: func(context.Context, string) (*sharedGCSClient, error) {
			return &sharedGCSClient{closeFunc: func() error { gcsCloses.Add(1); return nil }}, nil
		},
		runCamera: runner,
		clock: func() time.Time {
			return time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
		},
		stdout: stdout,
		stderr: stderr,
	}, &gcsCloses, stderr
}

func validDevice(id int64) deviceRecord {
	return deviceRecord{
		ID: id, Name: fmt.Sprintf("Camera %d", id), SiteID: 1, Status: "enabled",
		GCSURI:        fmt.Sprintf("gs://camera-bucket/site-1/device-%d/", id),
		RTSPConfig:    []byte(fmt.Sprintf(`{"uri":"rtsp://camera-%d.example/live","username":"user-%d","password":"password-%d"}`, id, id, id)),
		CaptureConfig: []byte(`{"fps":3,"change_threshold_percent":0.5}`),
	}
}

func disabledDevice(id int64) deviceRecord {
	device := validDevice(id)
	device.Status = "disabled"
	return device
}

func invalidDevice(id int64) deviceRecord {
	device := validDevice(id)
	device.RTSPConfig = []byte(`null`)
	return device
}

func waitForCommission(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("commissioning runtime did not stop")
		return nil
	}
}

func assertDeviceEvent(t *testing.T, events []gatewayEvent, deviceID int64, eventType string) {
	t.Helper()
	for _, event := range events {
		if event.Type == eventType && event.DeviceID != nil && *event.DeviceID == deviceID {
			return
		}
	}
	t.Fatalf("missing %s for Device %d in %#v", eventType, deviceID, events)
}

func copyDetails(details map[string]any) map[string]any {
	if details == nil {
		return nil
	}
	copyMap := make(map[string]any, len(details))
	for key, value := range details {
		copyMap[key] = value
	}
	return copyMap
}
