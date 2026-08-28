package connect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
)

// CameraConfig identifies one RTSP source and its GCS destination.
type CameraConfig struct {
	Version     int               `yaml:"version"`
	DeviceID    int64             `yaml:"device_id"`
	Name        string            `yaml:"name"`
	Source      SourceConfig      `yaml:"source"`
	Destination DestinationConfig `yaml:"destination"`
}

// SourceConfig contains the already-resolved RTSP source.
type SourceConfig struct {
	URI      string `yaml:"uri"`
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// DestinationConfig contains the GCS bucket and optional object prefix.
type DestinationConfig struct {
	GCSURI string `yaml:"gcs_uri"`
}

type gcsDestination struct {
	bucket string
	prefix string
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

// Connect captures one current RTSP frame and uploads one 1280x720 JPEG to GCS.
func Connect(ctx context.Context, config CameraConfig) error {
	if err := validateConfig(config); err != nil {
		return err
	}

	componentDir, err := componentDirectory()
	if err != nil {
		return err
	}
	credentialPath := filepath.Join(componentDir, "sa.json")
	if err := requireServiceAccount(credentialPath); err != nil {
		return err
	}

	rgb, err := captureRGBFrame(ctx, config.Source)
	if err != nil {
		return err
	}
	fmt.Println("RTSP connected")

	jpegData, err := encodeRGBFrame(rgb)
	if err != nil {
		return err
	}
	fmt.Println("Frame captured")

	destination, err := parseGCSURI(config.Destination.GCSURI)
	if err != nil {
		return errors.New("camera config invalid: destination.gcs_uri must be a gs URI with a bucket")
	}
	if err := uploadJPEG(ctx, destination, credentialPath, jpegData); err != nil {
		return err
	}
	fmt.Println("Frame uploaded")

	return nil
}

func validateConfig(config CameraConfig) error {
	if config.Version != 1 {
		return errors.New("camera config invalid: version must be 1")
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

	if strings.TrimSpace(config.Destination.GCSURI) == "" {
		return errors.New("camera config invalid: destination.gcs_uri must not be blank")
	}
	if _, err := parseGCSURI(config.Destination.GCSURI); err != nil {
		return errors.New("camera config invalid: destination.gcs_uri must be a gs URI with a bucket")
	}

	return nil
}

func componentDirectory() (string, error) {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("unable to locate connect component directory")
	}
	directory, err := filepath.Abs(filepath.Dir(sourceFile))
	if err != nil {
		return "", errors.New("unable to locate connect component directory")
	}
	return directory, nil
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

func ffmpegArguments(sourceURI string) []string {
	return []string{
		"-hide_banner",
		"-loglevel", "error",
		"-rtsp_transport", "tcp",
		"-i", sourceURI,
		"-map", "0:v:0",
		"-an",
		"-sn",
		"-dn",
		"-frames:v", "1",
		"-vf", "scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2:black,format=rgb24",
		"-f", "rawvideo",
		"-pix_fmt", "rgb24",
		"pipe:1",
	}
}

func captureRGBFrame(ctx context.Context, source SourceConfig) ([]byte, error) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, errors.New("frame capture/decode failed: ffmpeg not found on PATH")
	}

	sourceURI, err := authenticatedRTSPURI(source)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, ffmpegPath, ffmpegArguments(sourceURI)...)
	var stdout bytes.Buffer
	stderr := newCappedBuffer(ffmpegErrorLimit)
	cmd.Stdout = &stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, errors.New("RTSP connection failed: operation cancelled or timed out")
		}
		return nil, classifyFFmpegFailure(stderr.String())
	}
	if stdout.Len() != rawFrameSize {
		return nil, fmt.Errorf("frame capture/decode failed: expected %d RGB24 bytes, received %d", rawFrameSize, stdout.Len())
	}

	return stdout.Bytes(), nil
}

func classifyFFmpegFailure(diagnostic string) error {
	lower := strings.ToLower(diagnostic)
	authenticationMarkers := []string{
		"401 unauthorized",
		"401 (unauthorized)",
		"authentication failed",
		"server returned 401",
		"method describe failed: 401",
	}
	for _, marker := range authenticationMarkers {
		if strings.Contains(lower, marker) {
			return errors.New("RTSP authentication failed")
		}
	}

	connectionMarkers := []string{
		"connection refused",
		"connection timed out",
		"could not connect",
		"network is unreachable",
		"no route to host",
		"unable to open resource",
	}
	for _, marker := range connectionMarkers {
		if strings.Contains(lower, marker) {
			return errors.New("RTSP connection failed")
		}
	}

	return errors.New("frame capture/decode failed")
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
	filename := "connect-test-" + timestamp.UTC().Format("20060102T150405.000000000Z") + ".jpg"
	if prefix == "" {
		return filename
	}
	return prefix + "/" + filename
}

func uploadJPEG(ctx context.Context, destination gcsDestination, credentialPath string, jpegData []byte) error {
	client, err := storage.NewClient(ctx, option.WithCredentialsFile(credentialPath))
	if err != nil {
		return errors.New("GCS authentication failed")
	}
	defer client.Close()

	uploadContext, cancel := context.WithCancel(ctx)
	defer cancel()

	object := client.Bucket(destination.bucket).
		Object(objectName(destination.prefix, time.Now())).
		Retryer(storage.WithPolicy(storage.RetryNever)).
		If(storage.Conditions{DoesNotExist: true})
	writer := object.NewWriter(uploadContext)
	writer.ContentType = "image/jpeg"

	written, err := writer.Write(jpegData)
	if err != nil {
		cancel()
		return classifyGCSFailure(err)
	}
	if written != len(jpegData) {
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
