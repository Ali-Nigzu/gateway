package connect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"gopkg.in/yaml.v3"
)

const (
	frameWidth       = 1280
	frameHeight      = 720
	rgbBytesPerPixel = 3
	rawFrameSize     = frameWidth * frameHeight * rgbBytesPerPixel
	jpegQuality      = 90
	ffmpegErrorLimit = 32 * 1024
	objectTimeLayout = "2006-01-02T15-04-05.000000Z.jpg"
)

// CameraConfig identifies one RTSP source and its GCS destination.
type CameraConfig struct {
	Version     int               `yaml:"version"`
	DeviceID    int64             `yaml:"device_id"`
	Name        string            `yaml:"name"`
	Source      SourceConfig      `yaml:"source"`
	Capture     CaptureConfig     `yaml:"capture"`
	Destination DestinationConfig `yaml:"destination"`
}

// SourceConfig contains the already-resolved RTSP source.
type SourceConfig struct {
	URI      string `json:"uri" yaml:"uri"`
	Username string `json:"username" yaml:"username"`
	Password string `json:"password" yaml:"password"`
}

// CaptureConfig controls the fixed candidate rate and scene-change threshold.
type CaptureConfig struct {
	FPS                    int     `json:"fps" yaml:"fps"`
	ChangeThresholdPercent float64 `json:"change_threshold_percent" yaml:"change_threshold_percent"`
}

// DestinationConfig contains the GCS bucket and optional object prefix.
type DestinationConfig struct {
	GCSURI string `yaml:"gcs_uri"`
}

type gcsDestination struct {
	bucket string
	prefix string
}

type cameraCallbacks struct {
	ffmpegStarted func(context.Context, time.Time)
	ffmpegExited  func(context.Context, time.Time, *int)
	connected     func(context.Context, time.Time) error
	frameObserved func(context.Context, time.Time) error
	uploaded      func(context.Context, time.Time, time.Time) error
}

type cameraRunResult struct {
	successfulUploads int
}

// LoadConfig reads, decodes, and validates a camera configuration.
func LoadConfig(filename string) (CameraConfig, error) {
	var config CameraConfig

	data, err := os.ReadFile(filename)
	if err != nil {
		return config, errors.New("camera config invalid: unable to read camera.yml")
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		return config, errors.New("camera config invalid: unable to parse camera.yml")
	}
	if err := validateConfig(config); err != nil {
		return config, err
	}

	return config, nil
}

// Connect streams normalized RTSP frames, suppresses unchanged candidates, and
// uploads changed 1280x720 JPEGs until cancellation or a fatal error.
func Connect(ctx context.Context, config CameraConfig) error {
	if err := validateConfig(config); err != nil {
		return err
	}

	credentialPath, err := serviceAccountPath()
	if err != nil {
		return err
	}
	if err := requireServiceAccount(credentialPath); err != nil {
		return err
	}

	client, err := newStorageClient(ctx, credentialPath)
	if err != nil {
		return err
	}
	defer client.Close()

	result, streamErr := connectCamera(ctx, config, client, cameraCallbacks{
		connected: func(context.Context, time.Time) error {
			fmt.Println("RTSP connected")
			return nil
		},
		uploaded: func(context.Context, time.Time, time.Time) error {
			fmt.Println("Frame uploaded")
			return nil
		},
	}, time.Now)

	if ctx.Err() != nil && result.successfulUploads > 0 {
		fmt.Println("Stopped")
		return nil
	}
	if ctx.Err() != nil {
		return errors.New("stream stopped before first successful upload: operation cancelled")
	}
	return streamErr
}

func connectCamera(
	ctx context.Context,
	config CameraConfig,
	client *storage.Client,
	callbacks cameraCallbacks,
	clock func() time.Time,
) (cameraRunResult, error) {
	if err := validateConfig(config); err != nil {
		return cameraRunResult{}, err
	}

	destination, err := parseGCSURI(config.Destination.GCSURI)
	if err != nil {
		return cameraRunResult{}, errors.New("camera config invalid: destination.gcs_uri must be a gs URI with a bucket")
	}
	uploader := &gcsUploader{client: client, destination: destination}
	processor := newStreamProcessor(
		config.Capture.ChangeThresholdPercent,
		encodeRGBFrame,
		uploader.upload,
	)
	processor.clock = clock
	processor.afterUpload = callbacks.uploaded

	connected := false
	streamErr := streamRTSP(
		ctx,
		config.Source,
		config.Capture.FPS,
		clock,
		streamLifecycle{
			started: callbacks.ffmpegStarted,
			exited:  callbacks.ffmpegExited,
		},
		func(candidate frameCandidate) error {
			// FFmpeg exposes no reliable RTSP-handshake callback. The first
			// complete normalized frame is therefore the earliest trustworthy
			// boundary at which the stream can be called operational.
			if !connected {
				if callbacks.connected != nil {
					if err := callbacks.connected(ctx, candidate.observedAt); err != nil {
						return err
					}
				}
				connected = true
			}
			if callbacks.frameObserved != nil {
				if err := callbacks.frameObserved(ctx, candidate.observedAt); err != nil {
					return err
				}
			}
			_, err := processor.process(ctx, candidate)
			return err
		},
	)

	return cameraRunResult{successfulUploads: processor.successfulUploads}, streamErr
}

func validateConfig(config CameraConfig) error {
	if config.Version != 2 {
		return errors.New("camera config invalid: version must be 2")
	}
	if config.DeviceID <= 0 {
		return errors.New("camera config invalid: device_id must be greater than 0")
	}
	if strings.TrimSpace(config.Name) == "" {
		return errors.New("camera config invalid: name must not be blank")
	}

	rtspURI := strings.TrimSpace(config.Source.URI)
	if rtspURI == "" {
		return errors.New("camera config invalid: source.uri must not be blank")
	}
	parsedRTSP, err := url.Parse(rtspURI)
	if err != nil || !strings.EqualFold(parsedRTSP.Scheme, "rtsp") || parsedRTSP.Hostname() == "" {
		return errors.New("camera config invalid: source.uri must be an rtsp URI with a host")
	}

	if config.Capture.FPS != 3 {
		return errors.New("camera config invalid: capture.fps must be 3")
	}
	threshold := config.Capture.ChangeThresholdPercent
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) || threshold <= 0 || threshold > 100 {
		return errors.New("camera config invalid: capture.change_threshold_percent must be greater than 0 and at most 100")
	}

	if strings.TrimSpace(config.Destination.GCSURI) == "" {
		return errors.New("camera config invalid: destination.gcs_uri must not be blank")
	}
	if _, err := parseGCSURI(config.Destination.GCSURI); err != nil {
		return errors.New("camera config invalid: destination.gcs_uri must be a gs URI with a bucket")
	}

	return nil
}

func serviceAccountPath() (string, error) {
	filename, err := filepath.Abs("sa.json")
	if err != nil {
		return "", errors.New("sa.json is not accessible")
	}
	return filename, nil
}

func requireServiceAccount(filename string) error {
	info, err := os.Stat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("sa.json not found")
	}
	if err != nil {
		return errors.New("sa.json is not accessible")
	}
	if !info.Mode().IsRegular() {
		return errors.New("sa.json is not a regular file")
	}
	return nil
}

func authenticatedRTSPURI(source SourceConfig) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(source.URI))
	if err != nil || !strings.EqualFold(parsed.Scheme, "rtsp") || parsed.Hostname() == "" {
		return "", errors.New("camera config invalid: source.uri must be an rtsp URI with a host")
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
		return nil, fmt.Errorf("frame capture/decode failed: expected %d RGB24 bytes, received %d", rawFrameSize, len(rgb))
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
		return nil, errors.New("frame capture/decode failed: JPEG encoding failed")
	}
	return encoded.Bytes(), nil
}

func parseGCSURI(rawURI string) (gcsDestination, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURI))
	if err != nil || !strings.EqualFold(parsed.Scheme, "gs") || parsed.Host == "" {
		return gcsDestination{}, errors.New("invalid GCS URI")
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
		return nil, errors.New("GCS authentication failed")
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
		return classifyGCSFailure(err)
	}
	if written != len(jpegData) {
		_ = writer.CloseWithError(io.ErrShortWrite)
		cancel()
		return classifyGCSFailure(io.ErrShortWrite)
	}
	if err := writer.Close(); err != nil {
		return classifyGCSFailure(err)
	}

	return nil
}

func classifyGCSFailure(err error) error {
	var apiError *googleapi.Error
	if errors.As(err, &apiError) && (apiError.Code == http.StatusUnauthorized || apiError.Code == http.StatusForbidden) {
		return errors.New("GCS authentication failed")
	}
	return errors.New("GCS upload failed")
}

type cappedBuffer struct {
	buffer    bytes.Buffer
	remaining int
}

func newCappedBuffer(limit int) *cappedBuffer {
	return &cappedBuffer{remaining: limit}
}

func (buffer *cappedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	if buffer.remaining > 0 {
		toWrite := data
		if len(toWrite) > buffer.remaining {
			toWrite = toWrite[:buffer.remaining]
		}
		_, _ = buffer.buffer.Write(toWrite)
		buffer.remaining -= len(toWrite)
	}
	return originalLength, nil
}

func (buffer *cappedBuffer) String() string {
	return buffer.buffer.String()
}
