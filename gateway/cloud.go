package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

const (
	gcsBucketName    = "camos-prod-0"
	objectTimeLayout = "2006-01-02T15-04-05.000Z.jpg"
)

type acceptedImage struct {
	version    uint64
	capturedAt time.Time
	jpegBytes  []byte
}

type latestImageSlot struct {
	mutex     sync.Mutex
	version   uint64
	latest    acceptedImage
	hasLatest bool
	wake      chan struct{}
}

func newLatestImageSlot() latestImageSlot {
	return latestImageSlot{wake: make(chan struct{}, 1)}
}

func (slot *latestImageSlot) replace(capturedAt time.Time, jpegBytes []byte) {
	slot.mutex.Lock()
	slot.version++
	slot.latest = acceptedImage{
		version:    slot.version,
		capturedAt: capturedAt,
		jpegBytes:  jpegBytes,
	}
	slot.hasLatest = true
	slot.mutex.Unlock()
	select {
	case slot.wake <- struct{}{}:
	default:
	}
}

func (slot *latestImageSlot) snapshot() (acceptedImage, bool) {
	slot.mutex.Lock()
	defer slot.mutex.Unlock()
	return slot.latest, slot.hasLatest
}

func (slot *latestImageSlot) clear(version uint64) {
	slot.mutex.Lock()
	if slot.hasLatest && slot.latest.version == version {
		slot.latest = acceptedImage{}
		slot.hasLatest = false
	}
	slot.mutex.Unlock()
}

type runtimeFactState struct {
	mutex             sync.Mutex
	latestConnectedAt time.Time
	latestSeenAt      time.Time
	latestUploadedAt  time.Time
}

type runtimeFactSnapshot struct {
	connectedAt time.Time
	seenAt      time.Time
	uploadedAt  time.Time
}

func (facts *runtimeFactState) observeFrame(at time.Time, connected bool) {
	facts.mutex.Lock()
	if connected && at.After(facts.latestConnectedAt) {
		facts.latestConnectedAt = at
	}
	if at.After(facts.latestSeenAt) {
		facts.latestSeenAt = at
	}
	facts.mutex.Unlock()
}

func (facts *runtimeFactState) observeUpload(at time.Time) {
	facts.mutex.Lock()
	if at.After(facts.latestUploadedAt) {
		facts.latestUploadedAt = at
	}
	facts.mutex.Unlock()
}

func (facts *runtimeFactState) snapshot() runtimeFactSnapshot {
	facts.mutex.Lock()
	defer facts.mutex.Unlock()
	return runtimeFactSnapshot{
		connectedAt: facts.latestConnectedAt,
		seenAt:      facts.latestSeenAt,
		uploadedAt:  facts.latestUploadedAt,
	}
}

func superviseDeviceUploader(
	ctx context.Context,
	client *storage.Client,
	runtime *deviceRuntime,
) {
	for {
		runDeviceUploaderSafely(ctx, client, runtime)
		if ctx.Err() != nil || !waitContext(ctx, retryDelay) {
			return
		}
	}
}

func runDeviceUploaderSafely(
	ctx context.Context,
	client *storage.Client,
	runtime *deviceRuntime,
) {
	defer func() {
		_ = recover()
	}()
	uploadDeviceImages(ctx, client, runtime)
}

func uploadDeviceImages(
	ctx context.Context,
	client *storage.Client,
	runtime *deviceRuntime,
) {
	for {
		image, ok := runtime.images.snapshot()
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-runtime.images.wake:
				continue
			}
		}
		completedAt, err := uploadAcceptedImage(
			ctx,
			client,
			runtime.config,
			image,
		)
		if err == nil {
			runtime.facts.observeUpload(completedAt)
			runtime.images.clear(image.version)
			continue
		}
		if !waitContext(ctx, retryDelay) {
			return
		}
	}
}

func uploadAcceptedImage(
	ctx context.Context,
	client *storage.Client,
	device deviceRecord,
	image acceptedImage,
) (time.Time, error) {
	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	defer cancel()

	object := acceptedImageObject(client, device, image.capturedAt)
	writer := object.If(storage.Conditions{DoesNotExist: true}).NewWriter(operationCtx)
	writer.ContentType = "image/jpeg"
	writer.ChunkSize = 0

	written, writeErr := writer.Write(image.jpegBytes)
	if writeErr != nil {
		_ = writer.CloseWithError(writeErr)
		if isPreconditionFailed(writeErr) {
			return time.Now().UTC(), nil
		}
		return time.Time{}, writeErr
	}
	if written != len(image.jpegBytes) {
		_ = writer.CloseWithError(io.ErrShortWrite)
		return time.Time{}, io.ErrShortWrite
	}
	if err := writer.Close(); err != nil {
		if isPreconditionFailed(err) {
			return time.Now().UTC(), nil
		}
		return time.Time{}, err
	}
	return time.Now().UTC(), nil
}

func acceptedImageObject(
	client *storage.Client,
	device deviceRecord,
	capturedAt time.Time,
) *storage.ObjectHandle {
	return client.
		Bucket(gcsBucketName).
		Object(objectName(
			device.organisationID,
			device.siteID,
			device.id,
			capturedAt,
		)).
		Retryer(storage.WithPolicy(storage.RetryNever))
}

func objectName(
	organisationID int64,
	siteID int64,
	deviceID int64,
	timestamp time.Time,
) string {
	return strconv.FormatInt(organisationID, 10) + "/" +
		strconv.FormatInt(siteID, 10) + "/" +
		strconv.FormatInt(deviceID, 10) + "/" +
		timestamp.UTC().Format(objectTimeLayout)
}

func isPreconditionFailed(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusPreconditionFailed
}

func superviseRuntimeFacts(
	ctx context.Context,
	store *postgresStore,
	statement string,
	runtimes []*deviceRuntime,
) {
	arguments := make([]any, 4*len(runtimes))
	for {
		success := writeRuntimeFactsSafely(ctx, store, statement, arguments, runtimes)
		if ctx.Err() != nil {
			return
		}
		delay := retryDelay
		if success {
			delay = runtimeFactInterval
		}
		if !waitContext(ctx, delay) {
			return
		}
	}
}

func writeRuntimeFactsSafely(
	ctx context.Context,
	store *postgresStore,
	statement string,
	arguments []any,
	runtimes []*deviceRuntime,
) (success bool) {
	defer func() {
		if recover() != nil {
			success = false
		}
	}()

	for index, runtime := range runtimes {
		snapshot := runtime.facts.snapshot()
		argument := index * 4
		arguments[argument] = runtime.config.id
		arguments[argument+1] = nullableTime(snapshot.connectedAt)
		arguments[argument+2] = nullableTime(snapshot.seenAt)
		arguments[argument+3] = nullableTime(snapshot.uploadedAt)
	}
	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	defer cancel()
	err := store.writeRuntimeFacts(operationCtx, statement, arguments)
	return err == nil
}
