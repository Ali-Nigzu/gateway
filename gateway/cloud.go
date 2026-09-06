package main

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

const (
	gcsBucketName               = "camos-prod-0"
	framePackageUploadChunkSize = 1 << 20
)

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

func uploadFramePackage(
	ctx context.Context,
	client *storage.Client,
	device deviceRecord,
	framePackage framePackageUpload,
) (time.Time, error) {
	if client == nil {
		return time.Time{}, errors.New("cloud client is unavailable")
	}
	object := client.
		Bucket(gcsBucketName).
		Object(framePackageObjectName(
			device.organisationID,
			device.siteID,
			device.id,
			framePackage.window,
		)).
		Retryer(storage.WithPolicy(storage.RetryNever))
	writer := object.If(storage.Conditions{DoesNotExist: true}).NewWriter(ctx)
	writer.ContentType = "application/x-tar"
	writer.ChunkSize = framePackageUploadChunkSize
	writer.ChunkTransferTimeout = cloudOperationTimeout

	if writeErr := writeFramePackageTar(writer, framePackage); writeErr != nil {
		closeErr := writer.CloseWithError(writeErr)
		if isPreconditionFailed(writeErr) || isPreconditionFailed(closeErr) {
			return time.Now().UTC(), nil
		}
		return time.Time{}, writeErr
	}
	if err := writer.Close(); err != nil {
		if isPreconditionFailed(err) {
			return time.Now().UTC(), nil
		}
		return time.Time{}, err
	}
	return time.Now().UTC(), nil
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
		writeRuntimeFactsSafely(ctx, store, statement, arguments, runtimes)
		if ctx.Err() != nil {
			return
		}
		if !waitContext(ctx, retryDelay) {
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
) {
	defer func() {
		_ = recover()
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
	_ = store.writeRuntimeFacts(operationCtx, statement, arguments)
}
