package connect

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"io"
	"net/url"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

const (
	frameWidth       = 1280
	frameHeight      = 720
	rgbBytesPerPixel = 3
	rawFrameSize     = frameWidth * frameHeight * rgbBytesPerPixel
	jpegQuality      = 90
	objectTimeLayout = "2006-01-02T15-04-05.000000Z.jpg"
)

type cameraConfig struct {
	source  sourceConfig
	capture captureConfig
	gcsURI  string
}

type sourceConfig struct {
	URI      string `json:"uri"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type captureConfig struct {
	FPS                    int     `json:"fps"`
	ChangeThresholdPercent float64 `json:"change_threshold_percent"`
}

type gcsDestination struct {
	bucket string
	prefix string
}

func connectCamera(
	ctx context.Context,
	config cameraConfig,
	client *storage.Client,
	observer *deviceObserver,
) error {
	destination, err := parseGCSURI(config.gcsURI)
	if err != nil {
		return err
	}
	uploader := &gcsUploader{client: client, destination: destination}
	processor := newStreamProcessor(
		config.capture.ChangeThresholdPercent,
		uploader,
		observer,
	)

	connected := false
	return streamRTSP(
		ctx,
		config.source,
		config.capture.FPS,
		func(candidate frameCandidate) error {
			// FFmpeg exposes no reliable RTSP-handshake callback. The first
			// complete normalized frame is the earliest trustworthy evidence
			// that the stream is operational.
			if !connected {
				if err := observer.connected(ctx, candidate.observedAt); err != nil {
					return err
				}
				connected = true
			}
			if err := observer.frameObserved(ctx, candidate.observedAt); err != nil {
				return err
			}
			_, err := processor.process(ctx, candidate)
			return err
		},
	)
}

func authenticatedRTSPURI(source sourceConfig) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(source.URI))
	if err != nil {
		return "", deviceFailure{stage: failureStageStream}
	}

	if source.Username != "" || source.Password != "" {
		if source.Password == "" {
			parsed.User = url.User(source.Username)
		} else {
			parsed.User = url.UserPassword(source.Username, source.Password)
		}
	}
	return parsed.String(), nil
}

func encodeRGBFrame(rgb []byte) ([]byte, error) {
	if len(rgb) != rawFrameSize {
		return nil, deviceFailure{stage: failureStageStream}
	}

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
		return nil, deviceFailure{stage: failureStageStream}
	}
	return encoded.Bytes(), nil
}

func parseGCSURI(rawURI string) (gcsDestination, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURI))
	if err != nil {
		return gcsDestination{}, deviceFailure{stage: failureStageUpload}
	}
	return gcsDestination{
		bucket: parsed.Host,
		prefix: strings.Trim(parsed.Path, "/"),
	}, nil
}

func objectName(prefix string, timestamp time.Time) string {
	filename := timestamp.UTC().Format(objectTimeLayout)
	if prefix == "" {
		return filename
	}
	return prefix + "/" + filename
}

type gcsUploader struct {
	client      *storage.Client
	destination gcsDestination
}

func newStorageClient(ctx context.Context, credentialPath string) (*storage.Client, error) {
	client, err := storage.NewClient(ctx, option.WithCredentialsFile(credentialPath))
	if err != nil {
		return nil, errors.New("gateway startup failed")
	}
	return client, nil
}

func (uploader *gcsUploader) upload(ctx context.Context, jpegData []byte, observedAt time.Time) error {
	uploadContext, cancel := context.WithCancel(ctx)
	defer cancel()

	object := uploader.client.Bucket(uploader.destination.bucket).
		Object(objectName(uploader.destination.prefix, observedAt)).
		Retryer(storage.WithPolicy(storage.RetryNever)).
		If(storage.Conditions{DoesNotExist: true})
	writer := object.NewWriter(uploadContext)
	writer.ContentType = "image/jpeg"

	written, err := writer.Write(jpegData)
	if err != nil {
		_ = writer.CloseWithError(err)
		cancel()
		return deviceFailure{stage: failureStageUpload}
	}
	if written != len(jpegData) {
		_ = writer.CloseWithError(io.ErrShortWrite)
		cancel()
		return deviceFailure{stage: failureStageUpload}
	}
	if err := writer.Close(); err != nil {
		return deviceFailure{stage: failureStageUpload}
	}
	return nil
}
