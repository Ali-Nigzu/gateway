package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

func TestObjectNameUsesProductionHierarchyAndUTCMilliseconds(t *testing.T) {
	offset := time.FixedZone("UTC+2", 2*60*60)
	capturedAt := time.Date(2026, 8, 4, 13, 38, 55, 486999999, offset)

	got := objectName(42, 17, 83, capturedAt)
	want := "42/17/83/2026-08-04T11-38-55.486Z.jpg"
	if got != want {
		t.Fatalf("objectName() = %q, want %q", got, want)
	}
	filename := got[strings.LastIndex(got, "/")+1:]
	if strings.Contains(filename, ".486999") {
		t.Fatalf("object name has greater than millisecond precision: %q", got)
	}
}

func TestPreconditionFailureCompletesCreateWithoutRead(t *testing.T) {
	var (
		mutex    sync.Mutex
		requests []string
	)
	client := newTestStorageClient(t, http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		mutex.Lock()
		requests = append(requests, request.Method+" "+request.URL.Path)
		mutex.Unlock()
		if got := request.URL.Query().Get("ifGenerationMatch"); got != "0" {
			t.Errorf("ifGenerationMatch = %q, want 0", got)
		}
		writeStorageError(writer, http.StatusPreconditionFailed)
	}))

	completedAt, err := uploadAcceptedImage(
		context.Background(),
		client,
		deviceRecord{id: 83, organisationID: 42, siteID: 17},
		acceptedImage{
			capturedAt: time.Date(2026, 8, 4, 11, 38, 55, 486000000, time.UTC),
			jpegBytes:  []byte("jpeg"),
		},
	)
	if err != nil {
		t.Fatalf("uploadAcceptedImage() error = %v", err)
	}
	if completedAt.IsZero() {
		t.Fatal("precondition completion has a zero completion time")
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(requests) != 1 || !strings.HasPrefix(requests[0], "POST ") {
		t.Fatalf("requests = %#v, want one create request and no read", requests)
	}
}

func TestAmbiguousCreateRetryUsesSameNameThenAcceptsPrecondition(t *testing.T) {
	var (
		mutex    sync.Mutex
		requests []string
	)
	client := newTestStorageClient(t, http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		mutex.Lock()
		requests = append(
			requests,
			request.Method+" "+request.URL.Path+" "+
				request.URL.Query().Get("name")+" "+
				request.URL.Query().Get("ifGenerationMatch"),
		)
		requestCount := len(requests)
		mutex.Unlock()

		if requestCount == 1 {
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				t.Error("test server does not support connection hijacking")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("Hijack() error = %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		writeStorageError(writer, http.StatusPreconditionFailed)
	}))

	image := acceptedImage{
		capturedAt: time.Date(2026, 8, 4, 11, 38, 55, 486000000, time.UTC),
		jpegBytes:  []byte("jpeg"),
	}
	device := deviceRecord{id: 83, organisationID: 42, siteID: 17}
	if _, err := uploadAcceptedImage(context.Background(), client, device, image); err == nil {
		t.Fatal("ambiguous first create unexpectedly succeeded")
	}
	if _, err := uploadAcceptedImage(context.Background(), client, device, image); err != nil {
		t.Fatalf("same-name retry did not accept precondition result: %v", err)
	}

	mutex.Lock()
	defer mutex.Unlock()
	if len(requests) != 2 {
		t.Fatalf("requests = %#v, want exactly two create attempts", requests)
	}
	if requests[0] != requests[1] {
		t.Fatalf("retry changed deterministic object: %#v", requests)
	}
	wantSuffix := " 42/17/83/2026-08-04T11-38-55.486Z.jpg 0"
	if !strings.HasPrefix(requests[0], "POST /upload/storage/v1/b/"+gcsBucketName+"/o") ||
		!strings.HasSuffix(requests[0], wantSuffix) {
		t.Fatalf("unexpected create request = %q", requests[0])
	}
}

func newTestStorageClient(t *testing.T, handler http.Handler) *storage.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := storage.NewClient(
		context.Background(),
		option.WithEndpoint(server.URL),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("storage.NewClient() error = %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
	})
	return client
}

func writeStorageError(writer http.ResponseWriter, status int) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = fmt.Fprintf(
		writer,
		`{"error":{"code":%d,"message":"test error"}}`,
		status,
	)
}

func TestLatestImageReplacesPending(t *testing.T) {
	slot := newLatestImageSlot()
	slot.replace(time.Unix(1, 0), []byte("first"))
	first, ok := slot.snapshot()
	if !ok {
		t.Fatal("first image is missing")
	}
	slot.replace(time.Unix(2, 0), []byte("second"))

	latest, ok := slot.snapshot()
	if !ok {
		t.Fatal("latest image is missing")
	}
	if latest.version <= first.version {
		t.Fatalf("version did not advance: first=%d latest=%d", first.version, latest.version)
	}
	if !latest.capturedAt.Equal(time.Unix(2, 0)) || string(latest.jpegBytes) != "second" {
		t.Fatalf("unexpected latest image: %#v", latest)
	}
}

func TestOlderSuccessCannotClearNewer(t *testing.T) {
	slot := newLatestImageSlot()
	slot.replace(time.Unix(1, 0), []byte("old"))
	old, _ := slot.snapshot()
	slot.replace(time.Unix(2, 0), []byte("new"))

	slot.clear(old.version)

	latest, ok := slot.snapshot()
	if !ok || string(latest.jpegBytes) != "new" {
		t.Fatalf("newer image was cleared: %#v", latest)
	}
}

func TestMatchingSuccessClearsPending(t *testing.T) {
	slot := newLatestImageSlot()
	slot.replace(time.Unix(1, 0), []byte("image"))
	image, _ := slot.snapshot()

	slot.clear(image.version)

	if latest, ok := slot.snapshot(); ok {
		t.Fatalf("latest image was not cleared: %#v", latest)
	}
}

func TestPendingImageSurvivesUploaderPanic(t *testing.T) {
	runtime := &deviceRuntime{images: newLatestImageSlot()}
	runtime.images.replace(time.Unix(1, 0), []byte("pending"))
	before, _ := runtime.images.snapshot()

	runDeviceUploaderSafely(context.Background(), nil, runtime)

	after, ok := runtime.images.snapshot()
	if !ok || after.version != before.version || string(after.jpegBytes) != "pending" {
		t.Fatalf("pending image was lost: %#v", after)
	}
}

func TestLatestImageWakeNeverBlocks(t *testing.T) {
	slot := newLatestImageSlot()
	slot.replace(time.Unix(1, 0), []byte("first"))
	done := make(chan struct{})
	go func() {
		slot.replace(time.Unix(2, 0), []byte("second"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("producer blocked on wake delivery")
	}
	if len(slot.wake) != 1 {
		t.Fatalf("wake count = %d, want 1", len(slot.wake))
	}
	latest, ok := slot.snapshot()
	if !ok || string(latest.jpegBytes) != "second" {
		t.Fatalf("latest image was not replaced: %#v", latest)
	}
}
