package connect

import (
	"bytes"
	"errors"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const expectedCameraTemplate = `version: 1

device_id: 0
name: ""

source:
  uri: ""
  username: ""
  password: ""

destination:
  gcs_uri: ""
`

func representativeConfig() CameraConfig {
	return CameraConfig{
		Version:  1,
		DeviceID: 1,
		Name:     "Front Door",
		Source: SourceConfig{
			URI:      "rtsp://192.0.2.10/live",
			Username: "camera-user",
			Password: "camera-password",
		},
		Destination: DestinationConfig{
			GCSURI: "gs://camostesting/Orgs/Sites/Devices/TestCamera",
		},
	}
}

func TestCameraTemplateIsExact(t *testing.T) {
	data, err := os.ReadFile("camera.yml")
	if err != nil {
		t.Fatalf("read camera.yml: %v", err)
	}
	normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
	if normalized != expectedCameraTemplate {
		t.Fatalf("camera.yml does not match the required blank template\nwant:\n%s\ngot:\n%s", expectedCameraTemplate, data)
	}
}

func TestBlankCameraTemplateFailsValidation(t *testing.T) {
	_, err := LoadConfig("camera.yml")
	if err == nil || !strings.HasPrefix(err.Error(), "camera config invalid:") {
		t.Fatalf("LoadConfig(camera.yml) error = %v, want camera config invalid", err)
	}
}

func TestLoadConfigValid(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "camera.yml")
	data := `version: 1
device_id: 1
name: "Front Door"
source:
  uri: "rtsp://192.0.2.10/live"
  username: ""
  password: ""
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
	if config.Name != "Front Door" || config.DeviceID != 1 {
		t.Fatalf("LoadConfig() = %#v", config)
	}
}

func TestValidateConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CameraConfig)
	}{
		{name: "version", mutate: func(config *CameraConfig) { config.Version = 2 }},
		{name: "device", mutate: func(config *CameraConfig) { config.DeviceID = 0 }},
		{name: "name", mutate: func(config *CameraConfig) { config.Name = " " }},
		{name: "blank RTSP URI", mutate: func(config *CameraConfig) { config.Source.URI = "" }},
		{name: "RTSP scheme", mutate: func(config *CameraConfig) { config.Source.URI = "http://192.0.2.10/live" }},
		{name: "RTSP host", mutate: func(config *CameraConfig) { config.Source.URI = "rtsp:///live" }},
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

func TestFFmpegArguments(t *testing.T) {
	args := strings.Join(ffmpegArguments("rtsp://camera.example/live"), " ")
	for _, required := range []string{
		"-rtsp_transport tcp",
		"-frames:v 1",
		"scale=1280:720:force_original_aspect_ratio=decrease",
		"pad=1280:720:(ow-iw)/2:(oh-ih)/2:black",
		"format=rgb24",
		"-f rawvideo",
		"-pix_fmt rgb24",
		"pipe:1",
	} {
		if !strings.Contains(args, required) {
			t.Errorf("FFmpeg arguments missing %q", required)
		}
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
	rgb := make([]byte, rawFrameSize)
	for offset := 0; offset < len(rgb); offset += 3 {
		rgb[offset] = 210
		rgb[offset+1] = 80
		rgb[offset+2] = 35
	}

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

func TestObjectName(t *testing.T) {
	first := objectName("Orgs/Sites/Devices/TestCamera", time.Date(2026, 8, 28, 15, 30, 0, 123456789, time.UTC))
	second := objectName("Orgs/Sites/Devices/TestCamera", time.Date(2026, 8, 28, 15, 30, 0, 123456790, time.UTC))
	want := "Orgs/Sites/Devices/TestCamera/connect-test-20260828T153000.123456789Z.jpg"
	if first != want {
		t.Fatalf("objectName() = %q, want %q", first, want)
	}
	if first == second {
		t.Fatal("different timestamps produced the same object name")
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
