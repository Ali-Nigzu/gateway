package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"io"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/storage"
)

const (
	frameWidth       = 1280
	frameHeight      = 720
	rgbBytesPerPixel = 3
	rawFrameSize     = frameWidth * frameHeight * rgbBytesPerPixel
	jpegQuality      = 90
	objectTimeLayout = "2006-01-02T15-04-05.000000Z.jpg"
)

type deviceRuntime struct {
	siteID              int64
	deviceID            int64
	store               *postgresStore
	bucket              *storage.BucketHandle
	prefix              string
	threshold           float64
	comparisons         [2][comparisonCellCount]byte
	currentComparison   *[comparisonCellCount]byte
	baselineComparison  *[comparisonCellCount]byte
	hasBaseline         bool
	latestObservedAt    time.Time
	lastPersistedSeenAt time.Time
}

func (runtime *deviceRuntime) prepare(
	device deviceRecord,
	gcsClient *storage.Client,
) (string, int, error) {
	var source struct {
		URI      string `json:"uri"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(device.rtspConfig, &source); err != nil {
		return "", 0, failureStageStream
	}
	var capture struct {
		FPS       int     `json:"fps"`
		Threshold float64 `json:"change_threshold_percent"`
	}
	if err := json.Unmarshal(device.captureConfig, &capture); err != nil {
		return "", 0, failureStageStream
	}

	sourceURL, err := url.Parse(strings.TrimSpace(source.URI))
	if err != nil {
		return "", 0, failureStageStream
	}
	if source.Username != "" || source.Password != "" {
		if source.Password == "" {
			sourceURL.User = url.User(source.Username)
		} else {
			sourceURL.User = url.UserPassword(source.Username, source.Password)
		}
	}
	destination, err := url.Parse(strings.TrimSpace(device.gcsURI))
	if err != nil {
		return "", 0, failureStageStream
	}

	runtime.bucket = gcsClient.Bucket(destination.Host)
	runtime.prefix = strings.Trim(destination.Path, "/")
	runtime.threshold = capture.Threshold
	runtime.currentComparison = &runtime.comparisons[0]
	runtime.baselineComparison = &runtime.comparisons[1]
	return sourceURL.String(), capture.FPS, nil
}

func (runtime *deviceRuntime) run(ctx context.Context, sourceURI string, fps int) error {
	connected := false
	return streamRTSP(ctx, sourceURI, fps, func(rgb []byte, observedAt time.Time) error {
		// The first complete normalized frame is the earliest reliable
		// connection evidence exposed by the FFmpeg pipeline.
		if !connected {
			if err := runtime.connected(ctx, observedAt); err != nil {
				return err
			}
			connected = true
		}
		if err := runtime.frameObserved(ctx, observedAt); err != nil {
			return err
		}
		return runtime.process(ctx, rgb, observedAt)
	})
}

func (runtime *deviceRuntime) process(ctx context.Context, rgb []byte, observedAt time.Time) error {
	fillComparison(rgb, runtime.currentComparison)
	if runtime.hasBaseline && !materiallyChanged(
		runtime.baselineComparison,
		runtime.currentComparison,
		runtime.threshold,
	) {
		return nil
	}

	jpegData, err := encodeRGBFrame(rgb)
	if err != nil {
		return err
	}
	if err := runtime.upload(ctx, jpegData, observedAt); err != nil {
		return err
	}

	// Baseline advances after GCS success and remains advanced if the following
	// database write fails, preventing a duplicate object write.
	runtime.baselineComparison, runtime.currentComparison =
		runtime.currentComparison, runtime.baselineComparison
	runtime.hasBaseline = true
	completedAt := time.Now().UTC()
	return runtime.uploaded(ctx, observedAt, completedAt)
}

func encodeRGBFrame(rgb []byte) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, frameWidth, frameHeight))
	sourceOffset := 0
	for y := 0; y < frameHeight; y++ {
		for x := 0; x < frameWidth; x++ {
			destinationOffset := y*img.Stride + x*4
			img.Pix[destinationOffset] = rgb[sourceOffset]
			img.Pix[destinationOffset+1] = rgb[sourceOffset+1]
			img.Pix[destinationOffset+2] = rgb[sourceOffset+2]
			img.Pix[destinationOffset+3] = 0xff
			sourceOffset += rgbBytesPerPixel
		}
	}

	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, failureStageStream
	}
	return encoded.Bytes(), nil
}

func (runtime *deviceRuntime) upload(ctx context.Context, jpegData []byte, observedAt time.Time) error {
	object := runtime.bucket.
		Object(objectName(runtime.prefix, observedAt)).
		Retryer(storage.WithPolicy(storage.RetryNever)).
		If(storage.Conditions{DoesNotExist: true})
	writer := object.NewWriter(ctx)
	writer.ContentType = "image/jpeg"

	written, err := writer.Write(jpegData)
	if err != nil {
		_ = writer.CloseWithError(err)
		return failureStageUpload
	}
	if written != len(jpegData) {
		_ = writer.CloseWithError(io.ErrShortWrite)
		return failureStageUpload
	}
	if err := writer.Close(); err != nil {
		return failureStageUpload
	}
	return nil
}

func objectName(prefix string, timestamp time.Time) string {
	filename := timestamp.Format(objectTimeLayout)
	if prefix == "" {
		return filename
	}
	return prefix + "/" + filename
}

func (runtime *deviceRuntime) connected(ctx context.Context, at time.Time) error {
	runtime.observe(at)
	if err := runtime.store.markConnected(ctx, runtime.siteID, runtime.deviceID, at); err != nil {
		return failureStageDatabase
	}
	runtime.lastPersistedSeenAt = laterTime(runtime.lastPersistedSeenAt, at)
	return nil
}

func (runtime *deviceRuntime) frameObserved(ctx context.Context, at time.Time) error {
	runtime.observe(at)
	if runtime.lastPersistedSeenAt.IsZero() ||
		runtime.latestObservedAt.Sub(runtime.lastPersistedSeenAt) >= time.Minute {
		return runtime.persistLatestSeen(ctx)
	}
	return nil
}

func (runtime *deviceRuntime) uploaded(ctx context.Context, capturedAt, completedAt time.Time) error {
	runtime.observe(capturedAt)
	if err := runtime.store.markUploaded(
		ctx,
		runtime.siteID,
		runtime.deviceID,
		capturedAt,
		completedAt,
	); err != nil {
		return failureStageDatabase
	}
	runtime.lastPersistedSeenAt = laterTime(runtime.lastPersistedSeenAt, capturedAt)
	return nil
}

func (runtime *deviceRuntime) flush(ctx context.Context) error {
	if runtime.latestObservedAt.IsZero() ||
		!runtime.latestObservedAt.After(runtime.lastPersistedSeenAt) {
		return nil
	}
	return runtime.persistLatestSeen(ctx)
}

func (runtime *deviceRuntime) observe(at time.Time) {
	if at.After(runtime.latestObservedAt) {
		runtime.latestObservedAt = at
	}
}

func (runtime *deviceRuntime) persistLatestSeen(ctx context.Context) error {
	if err := runtime.store.markFrameSeen(
		ctx,
		runtime.siteID,
		runtime.deviceID,
		runtime.latestObservedAt,
	); err != nil {
		return failureStageDatabase
	}
	runtime.lastPersistedSeenAt = runtime.latestObservedAt
	return nil
}

func laterTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}
