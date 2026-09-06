package main

import (
	"bytes"
	"image/jpeg"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestDeviceSourceURIUsesFlattenedCredentials(t *testing.T) {
	config := deviceRecord{
		rtspURI:      "  rtsp://camera.example/live  ",
		rtspUsername: "camera user",
		rtspPassword: "p@ss/word",
	}

	source, err := deviceSourceURI(config)
	if err != nil {
		t.Fatalf("deviceSourceURI() error = %v", err)
	}
	parsed, err := url.Parse(source)
	if err != nil {
		t.Fatalf("generated source URI does not parse: %v", err)
	}
	if parsed.Scheme != "rtsp" || parsed.Host != "camera.example" || parsed.Path != "/live" {
		t.Fatalf("unexpected source URI = %q", source)
	}
	if parsed.User == nil || parsed.User.Username() != "camera user" {
		t.Fatalf("unexpected source username in %q", source)
	}
	password, ok := parsed.User.Password()
	if !ok || password != "p@ss/word" {
		t.Fatalf("unexpected source password in %q", source)
	}
}

func TestDeviceSourceURIWithoutCredentialsPreservesSource(t *testing.T) {
	source, err := deviceSourceURI(deviceRecord{
		rtspURI: "rtsp://camera.example:8554/stream?profile=main",
	})
	if err != nil {
		t.Fatalf("deviceSourceURI() error = %v", err)
	}
	if source != "rtsp://camera.example:8554/stream?profile=main" {
		t.Fatalf("source = %q", source)
	}
}

func TestDeviceSourceURIRejectsMalformedSource(t *testing.T) {
	if _, err := deviceSourceURI(deviceRecord{rtspURI: "rtsp://camera/%zz"}); err == nil {
		t.Fatal("deviceSourceURI() accepted malformed URI")
	}
}

func TestProcessFrameStagesEveryAcceptedJPEG(t *testing.T) {
	cache := newTestFramePackageCache(t, 15)
	runtime := newDeviceRuntime(deviceRecord{}, cache)
	frame := make([]byte, rawFrameSize)
	firstAt := time.Date(2026, 9, 6, 11, 7, 3, 0, time.UTC)
	if err := runtime.processFrame(frame, firstAt, 1); err != nil {
		t.Fatal(err)
	}
	if !runtime.hasBaseline {
		t.Fatal("first accepted frame did not establish a baseline")
	}
	if err := runtime.processFrame(frame, firstAt.Add(time.Second), 1); err != nil {
		t.Fatal(err)
	}
	for index := range frame {
		frame[index] = 0xff
	}
	if err := runtime.processFrame(frame, firstAt.Add(2*time.Second), 1); err != nil {
		t.Fatal(err)
	}

	cache.mutex.Lock()
	frames := append([]framePackageFrame(nil), cache.current.frames...)
	cache.mutex.Unlock()
	if len(frames) != 2 {
		t.Fatalf("staged frame count = %d, want 2 accepted frames", len(frames))
	}
	for _, staged := range frames {
		encoded, err := os.ReadFile(staged.path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := jpeg.Decode(bytes.NewReader(encoded)); err != nil {
			t.Fatalf("staged payload is not a JPEG: %v", err)
		}
	}
}

func TestProcessFrameDoesNotAdvanceBaselineWhenPackageWriteFails(t *testing.T) {
	runtime := newDeviceRuntime(deviceRecord{}, nil)
	if err := runtime.processFrame(
		make([]byte, rawFrameSize),
		time.Now().UTC(),
		1,
	); err == nil {
		t.Fatal("missing package cache was accepted")
	}
	if runtime.hasBaseline {
		t.Fatal("failed package write advanced the change baseline")
	}
}
