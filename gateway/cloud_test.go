package main

import (
	"context"
	"testing"
	"time"
)

func TestLatestImageReplacesPending(t *testing.T) {
	slot := newLatestImageSlot()
	slot.replace(time.Unix(1, 0), []byte("first"))
	first, ok := slot.snapshot()
	if !ok {
		t.Fatal("first image is missing")
	}
	slot.replace(time.Unix(2, 0), []byte("second"))

	latest, ok := slot.snapshot()
	if !ok {
		t.Fatal("latest image is missing")
	}
	if latest.version <= first.version {
		t.Fatalf("version did not advance: first=%d latest=%d", first.version, latest.version)
	}
	if !latest.capturedAt.Equal(time.Unix(2, 0)) || string(latest.jpegBytes) != "second" {
		t.Fatalf("unexpected latest image: %#v", latest)
	}
}

func TestOlderSuccessCannotClearNewer(t *testing.T) {
	slot := newLatestImageSlot()
	slot.replace(time.Unix(1, 0), []byte("old"))
	old, _ := slot.snapshot()
	slot.replace(time.Unix(2, 0), []byte("new"))

	slot.clear(old.version)

	latest, ok := slot.snapshot()
	if !ok || string(latest.jpegBytes) != "new" {
		t.Fatalf("newer image was cleared: %#v", latest)
	}
}

func TestMatchingSuccessClearsPending(t *testing.T) {
	slot := newLatestImageSlot()
	slot.replace(time.Unix(1, 0), []byte("image"))
	image, _ := slot.snapshot()

	slot.clear(image.version)

	if latest, ok := slot.snapshot(); ok {
		t.Fatalf("latest image was not cleared: %#v", latest)
	}
}

func TestPendingImageSurvivesUploaderPanic(t *testing.T) {
	runtime := &deviceRuntime{images: newLatestImageSlot()}
	runtime.images.replace(time.Unix(1, 0), []byte("pending"))
	before, _ := runtime.images.snapshot()

	runDeviceUploaderSafely(context.Background(), nil, runtime)

	after, ok := runtime.images.snapshot()
	if !ok || after.version != before.version || string(after.jpegBytes) != "pending" {
		t.Fatalf("pending image was lost: %#v", after)
	}
}

func TestLatestImageWakeNeverBlocks(t *testing.T) {
	slot := newLatestImageSlot()
	slot.replace(time.Unix(1, 0), []byte("first"))
	done := make(chan struct{})
	go func() {
		slot.replace(time.Unix(2, 0), []byte("second"))
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("producer blocked on wake delivery")
	}
	if len(slot.wake) != 1 {
		t.Fatalf("wake count = %d, want 1", len(slot.wake))
	}
	latest, ok := slot.snapshot()
	if !ok || string(latest.jpegBytes) != "second" {
		t.Fatalf("latest image was not replaced: %#v", latest)
	}
}
