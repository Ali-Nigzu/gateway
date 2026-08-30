package connect

import (
	"bytes"
	"errors"
	"image/color"
	"image/jpeg"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const expectedCameraTemplate = `version: 2

device_id: 1
name: "Laptop Webcam"

source:
  uri: "rtsp://127.0.0.1:8554/cam"
  username: ""
  password: ""

capture:
  fps: 3
  change_threshold_percent: 0.5

destination:
  gcs_uri: "gs://camostesting/Orgs/Sites/Devices/TestCamera"
`

func representativeConfig() CameraConfig {
	return CameraConfig{
		Version:  2,
		DeviceID: 1,
		Name:     "Front Door",
		Source: SourceConfig{
			URI:      "rtsp://192.0.2.10/live",
			Username: "camera-user",
			Password: "camera-password",
		},
		Capture: CaptureConfig{
			FPS:                    3,
			ChangeThresholdPercent: 0.5,
		},
		Destination: DestinationConfig{
			GCSURI: "gs://camostesting/Orgs/Sites/Devices/TestCamera",
		},
	}
}

func TestCameraTemplateIsExactAndValid(t *testing.T) {
	data, err := os.ReadFile("camera.yml")
	if err != nil {
		t.Fatalf("read camera.yml: %v", err)
	}
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	if normalized != expectedCameraTemplate {
		t.Fatalf("camera.yml does not match the required Phase 2 template\nwant:\n%s\ngot:\n%s", expectedCameraTemplate, data)
	}

	config, err := LoadConfig("camera.yml")
	if err != nil {
		t.Fatalf("LoadConfig(camera.yml) error = %v", err)
	}
	if config.Capture.FPS != 3 || config.Capture.ChangeThresholdPercent != 0.5 {
		t.Fatalf("capture config = %#v", config.Capture)
	}
}

func TestLoadConfigValid(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "camera.yml")
	data := `version: 2
device_id: 1
name: "Front Door"
source:
  uri: "rtsp://192.0.2.10/live"
  username: ""
  password: ""
capture:
  fps: 3
  change_threshold_percent: 0.5
destination:
  gcs_uri: "gs://camostesting/Orgs/Sites/Devices/TestCamera"
`
	if err := os.WriteFile(filename, []byte(data), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	config, err := LoadConfig(filename)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if config.Version != 2 || config.Name != "Front Door" || config.Capture.FPS != 3 {
		t.Fatalf("LoadConfig() = %#v", config)
	}
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CameraConfig)
	}{
		{name: "version", mutate: func(config *CameraConfig) { config.Version = 1 }},
		{name: "device", mutate: func(config *CameraConfig) { config.DeviceID = 0 }},
		{name: "name", mutate: func(config *CameraConfig) { config.Name = " " }},
		{name: "blank RTSP URI", mutate: func(config *CameraConfig) { config.Source.URI = "" }},
		{name: "RTSP scheme", mutate: func(config *CameraConfig) { config.Source.URI = "http://192.0.2.10/live" }},
		{name: "RTSP host", mutate: func(config *CameraConfig) { config.Source.URI = "rtsp:///live" }},
		{name: "capture FPS low", mutate: func(config *CameraConfig) { config.Capture.FPS = 2 }},
		{name: "capture FPS high", mutate: func(config *CameraConfig) { config.Capture.FPS = 4 }},
		{name: "zero threshold", mutate: func(config *CameraConfig) { config.Capture.ChangeThresholdPercent = 0 }},
		{name: "negative threshold", mutate: func(config *CameraConfig) { config.Capture.ChangeThresholdPercent = -0.1 }},
		{name: "threshold above 100", mutate: func(config *CameraConfig) { config.Capture.ChangeThresholdPercent = 100.1 }},
		{name: "NaN threshold", mutate: func(config *CameraConfig) { config.Capture.ChangeThresholdPercent = math.NaN() }},
		{name: "blank GCS URI", mutate: func(config *CameraConfig) { config.Destination.GCSURI = "" }},
		{name: "GCS scheme", mutate: func(config *CameraConfig) { config.Destination.GCSURI = "https://bucket/prefix" }},
		{name: "GCS bucket", mutate: func(config *CameraConfig) { config.Destination.GCSURI = "gs:///prefix" }},
	}

	if err := validateConfig(representativeConfig()); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := representativeConfig()
			test.mutate(&config)
			if err := validateConfig(config); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}

func TestParseGCSURI(t *testing.T) {
	destination, err := parseGCSURI("gs://camostesting/Orgs/Sites/Devices/TestCamera")
	if err != nil {
		t.Fatalf("parseGCSURI() error = %v", err)
	}
	want := gcsDestination{bucket: "camostesting", prefix: "Orgs/Sites/Devices/TestCamera"}
	if !reflect.DeepEqual(destination, want) {
		t.Fatalf("parseGCSURI() = %#v, want %#v", destination, want)
	}
}

func TestAuthenticatedRTSPURI(t *testing.T) {
	source := SourceConfig{
		URI:      "rtsp://camera.example/live",
		Username: "name@example.com",
		Password: "p@ss:/word",
	}
	authenticated, err := authenticatedRTSPURI(source)
	if err != nil {
		t.Fatalf("authenticatedRTSPURI() error = %v", err)
	}
	if !strings.Contains(authenticated, "name%40example.com:p%40ss%3A%2Fword@") {
		t.Fatalf("authenticatedRTSPURI() did not safely escape credentials")
	}
	if source.URI != "rtsp://camera.example/live" {
		t.Fatalf("authenticatedRTSPURI() mutated source URI")
	}
}

func TestFFmpegErrorsDoNotLeakCredentials(t *testing.T) {
	diagnostic := "Unable to open rtsp://camera-user:camera-password@camera.example/live: authentication failed"
	err := classifyFFmpegFailure(diagnostic)
	if err.Error() != "RTSP authentication failed" {
		t.Fatalf("classifyFFmpegFailure() = %q", err)
	}
	if strings.Contains(err.Error(), "camera-user") || strings.Contains(err.Error(), "camera-password") {
		t.Fatalf("error leaked credentials: %q", err)
	}
}

func TestEncodeRGBFrameRejectsWrongSize(t *testing.T) {
	if _, err := encodeRGBFrame(make([]byte, rawFrameSize-1)); err == nil {
		t.Fatal("encodeRGBFrame accepted an incomplete RGB24 frame")
	}
}

func TestEncodeRGBFrameProduces1280x720JPEG(t *testing.T) {
	rgb := solidRGBFrame(210, 80, 35)
	encoded, err := encodeRGBFrame(rgb)
	if err != nil {
		t.Fatalf("encodeRGBFrame() error = %v", err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("decode JPEG: %v", err)
	}
	if decoded.Bounds().Dx() != frameWidth || decoded.Bounds().Dy() != frameHeight {
		t.Fatalf("JPEG dimensions = %dx%d", decoded.Bounds().Dx(), decoded.Bounds().Dy())
	}

	want := color.RGBA{R: 210, G: 80, B: 35, A: 255}
	got := color.RGBAModel.Convert(decoded.At(frameWidth/2, frameHeight/2)).(color.RGBA)
	if difference(got.R, want.R) > 5 || difference(got.G, want.G) > 5 || difference(got.B, want.B) > 5 {
		t.Fatalf("JPEG centre colour = %#v, want approximately %#v", got, want)
	}
}

func TestObjectNameUsesExactLoaderFormat(t *testing.T) {
	timestamp := time.Date(2026, 8, 30, 15, 3, 27, 123456789, time.FixedZone("BST", 60*60))
	got := objectName("Orgs/Sites/Devices/TestCamera", timestamp)
	want := "Orgs/Sites/Devices/TestCamera/2026-08-30T14-03-27.123456Z.jpg"
	if got != want {
		t.Fatalf("objectName() = %q, want %q", got, want)
	}
	basename := filepath.Base(got)
	if strings.Contains(basename, "connect-test-") {
		t.Fatalf("temporary Phase 1 prefix remains in %q", basename)
	}
	if strings.TrimSuffix(basename, ".jpg")+".jpg" != basename || strings.Count(basename, ".") != 2 {
		t.Fatalf("filename has an unexpected suffix: %q", basename)
	}
}

func TestMissingServiceAccountFailsClearly(t *testing.T) {
	err := requireServiceAccount(filepath.Join(t.TempDir(), "sa.json"))
	if err == nil || err.Error() != "sa.json not found" {
		t.Fatalf("requireServiceAccount() error = %v", err)
	}
}

func TestWindowsScriptRunsExpectedCommand(t *testing.T) {
	data, err := os.ReadFile("test.cmd")
	if err != nil {
		t.Fatalf("read test.cmd: %v", err)
	}
	script := strings.ToLower(strings.ReplaceAll(string(data), "\r\n", "\n"))
	for _, expected := range []string{"pushd \"%~dp0\"", "go run ./cmd/test", "exit /b %exit_code%"} {
		if !strings.Contains(script, expected) {
			t.Errorf("test.cmd is missing %q", expected)
		}
	}
}

func TestGCSFailureDoesNotExposeDetails(t *testing.T) {
	secret := errors.New("private-key-data")
	err := classifyGCSFailure(secret)
	if err.Error() != "GCS upload failed" || strings.Contains(err.Error(), "private-key-data") {
		t.Fatalf("classifyGCSFailure() = %q", err)
	}
}

func difference(left, right uint8) int {
	if left > right {
		return int(left - right)
	}
	return int(right - left)
}
