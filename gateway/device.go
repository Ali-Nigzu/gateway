package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/url"
	"strings"
	"time"
)

const (
	frameWidth       = 1280
	frameHeight      = 720
	rgbBytesPerPixel = 3
	rawFrameSize     = frameWidth * frameHeight * rgbBytesPerPixel
	jpegQuality      = 90
)

type deviceRuntime struct {
	config             deviceRecord
	rawFrame           []byte
	comparisons        [2][comparisonCellCount]byte
	currentComparison  *[comparisonCellCount]byte
	baselineComparison *[comparisonCellCount]byte
	hasBaseline        bool
	images             latestImageSlot
	facts              runtimeFactState
}

func newDeviceRuntime(config deviceRecord) *deviceRuntime {
	runtime := &deviceRuntime{
		config:   config,
		rawFrame: make([]byte, rawFrameSize),
		images:   newLatestImageSlot(),
	}
	runtime.currentComparison = &runtime.comparisons[0]
	runtime.baselineComparison = &runtime.comparisons[1]
	return runtime
}

func superviseDevice(ctx context.Context, runtime *deviceRuntime) {
	for {
		runDeviceAttemptSafely(ctx, runtime)
		if ctx.Err() != nil || !waitContext(ctx, retryDelay) {
			return
		}
	}
}

func runDeviceAttemptSafely(ctx context.Context, runtime *deviceRuntime) {
	defer func() {
		_ = recover()
	}()
	_ = runtime.runDeviceAttempt(ctx)
}

func (runtime *deviceRuntime) runDeviceAttempt(ctx context.Context) error {
	var source struct {
		URI      string `json:"uri"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.Unmarshal(runtime.config.rtspConfig, &source); err != nil {
		return err
	}
	var capture struct {
		FPS       int     `json:"fps"`
		Threshold float64 `json:"change_threshold_percent"`
	}
	if err := json.Unmarshal(runtime.config.captureConfig, &capture); err != nil {
		return err
	}

	sourceURL, err := url.Parse(strings.TrimSpace(source.URI))
	if err != nil {
		return err
	}
	if source.Username != "" || source.Password != "" {
		if source.Password == "" {
			sourceURL.User = url.User(source.Username)
		} else {
			sourceURL.User = url.UserPassword(source.Username, source.Password)
		}
	}

	connected := false
	return streamRTSP(
		ctx,
		sourceURL.String(),
		capture.FPS,
		runtime.rawFrame,
		func(rgb []byte, observedAt time.Time) error {
			runtime.facts.observeFrame(observedAt, !connected)
			connected = true
			return runtime.processFrame(rgb, observedAt, capture.Threshold)
		},
	)
}

func (runtime *deviceRuntime) processFrame(
	rgb []byte,
	observedAt time.Time,
	threshold float64,
) error {
	fillComparison(rgb, runtime.currentComparison)
	if runtime.hasBaseline && !materiallyChanged(
		runtime.baselineComparison,
		runtime.currentComparison,
		threshold,
	) {
		return nil
	}

	jpegData, err := encodeRGBFrame(rgb)
	if err != nil {
		return err
	}
	runtime.images.replace(observedAt, jpegData)
	runtime.baselineComparison, runtime.currentComparison =
		runtime.currentComparison, runtime.baselineComparison
	runtime.hasBaseline = true
	return nil
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
		return nil, err
	}
	return encoded.Bytes(), nil
}
