package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

const objectTimeLayout = "2006-01-02T15-04-05.000000Z.jpg"

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
	var uncertainVersion uint64
	for {
		image, ok := runtime.images.snapshot()
		if !ok {
			uncertainVersion = 0
			select {
			case <-ctx.Done():
				return
			case <-runtime.images.wake:
				continue
			}
		}
		if uncertainVersion != 0 && uncertainVersion != image.version {
			uncertainVersion = 0
		}

		if uncertainVersion != 0 && uncertainVersion == image.version {
			confirmedAt, exists, err := confirmAcceptedImage(
				ctx,
				client,
				runtime.config.gcsURI,
				image,
			)
			if err != nil {
				if !waitContext(ctx, retryDelay) {
					return
				}
				continue
			}
			if exists {
				runtime.facts.observeUpload(confirmedAt)
				runtime.images.clear(image.version)
				uncertainVersion = 0
				continue
			}
			uncertainVersion = 0
			continue
		}

		completedAt, ambiguous, err := uploadAcceptedImage(
			ctx,
			client,
			runtime.config.gcsURI,
			image,
		)
		if err == nil {
			runtime.facts.observeUpload(completedAt)
			runtime.images.clear(image.version)
			uncertainVersion = 0
			continue
		}
		if ambiguous {
			uncertainVersion = image.version
		} else {
			uncertainVersion = 0
		}
		if !waitContext(ctx, retryDelay) {
			return
		}
	}
}

func uploadAcceptedImage(
	ctx context.Context,
	client *storage.Client,
	gcsURI string,
	image acceptedImage,
) (time.Time, bool, error) {
	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	defer cancel()

	object, err := acceptedImageObject(client, gcsURI, image.capturedAt)
	if err != nil {
		return time.Time{}, false, err
	}
	writer := object.If(storage.Conditions{DoesNotExist: true}).NewWriter(operationCtx)
	writer.ContentType = "image/jpeg"
	writer.ChunkSize = 0

	written, writeErr := writer.Write(image.jpegBytes)
	if writeErr != nil {
		_ = writer.CloseWithError(writeErr)
		cancel()
		closeErr := writer.Close()
		ambiguous := isPreconditionFailed(writeErr) ||
			isPreconditionFailed(closeErr) ||
			written == len(image.jpegBytes)
		return time.Time{}, ambiguous, writeErr
	}
	if written != len(image.jpegBytes) {
		_ = writer.CloseWithError(io.ErrShortWrite)
		cancel()
		_ = writer.Close()
		return time.Time{}, false, io.ErrShortWrite
	}
	if err := writer.Close(); err != nil {
		return time.Time{}, uploadCompletionMayBeAmbiguous(err), err
	}
	return time.Now().UTC(), false, nil
}

func confirmAcceptedImage(
	ctx context.Context,
	client *storage.Client,
	gcsURI string,
	image acceptedImage,
) (time.Time, bool, error) {
	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	defer cancel()

	object, err := acceptedImageObject(client, gcsURI, image.capturedAt)
	if err != nil {
		return time.Time{}, false, err
	}
	attributes, err := object.Attrs(operationCtx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return attributes.Updated.UTC(), true, nil
}

func acceptedImageObject(
	client *storage.Client,
	gcsURI string,
	capturedAt time.Time,
) (*storage.ObjectHandle, error) {
	destination, err := url.Parse(strings.TrimSpace(gcsURI))
	if err != nil {
		return nil, err
	}
	prefix := strings.Trim(destination.Path, "/")
	return client.
		Bucket(destination.Host).
		Object(objectName(prefix, capturedAt)).
		Retryer(storage.WithPolicy(storage.RetryNever)), nil
}

func objectName(prefix string, timestamp time.Time) string {
	filename := timestamp.Format(objectTimeLayout)
	if prefix == "" {
		return filename
	}
	return prefix + "/" + filename
}

func isPreconditionFailed(err error) bool {
	var apiErr *googleapi.Error
	return errors.As(err, &apiErr) && apiErr.Code == http.StatusPreconditionFailed
}

func uploadCompletionMayBeAmbiguous(err error) bool {
	var apiErr *googleapi.Error
	if !errors.As(err, &apiErr) {
		return true
	}
	return apiErr.Code == http.StatusRequestTimeout ||
		apiErr.Code == http.StatusPreconditionFailed ||
		apiErr.Code >= http.StatusInternalServerError
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
