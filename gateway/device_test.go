package main

import (
	"net/url"
	"testing"
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
