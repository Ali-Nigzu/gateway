package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCanonicalFramePackageWindow(t *testing.T) {
	offset := time.FixedZone("UTC+2", 2*60*60)
	tests := []struct {
		name       string
		capturedAt time.Time
		minutes    int
		start      string
		end        string
	}{
		{
			name:       "one minute midway",
			capturedAt: time.Date(2026, 9, 6, 11, 7, 31, 999000000, time.UTC),
			minutes:    1,
			start:      "2026-09-06T11:07:00Z",
			end:        "2026-09-06T11:08:00Z",
		},
		{
			name:       "five minutes from offset timezone",
			capturedAt: time.Date(2026, 9, 6, 13, 7, 0, 0, offset),
			minutes:    5,
			start:      "2026-09-06T11:05:00Z",
			end:        "2026-09-06T11:10:00Z",
		},
		{
			name:       "fifteen minutes midway",
			capturedAt: time.Date(2026, 9, 6, 11, 7, 0, 0, time.UTC),
			minutes:    15,
			start:      "2026-09-06T11:00:00Z",
			end:        "2026-09-06T11:15:00Z",
		},
		{
			name:       "exact boundary enters next window",
			capturedAt: time.Date(2026, 9, 6, 11, 15, 0, 0, time.UTC),
			minutes:    15,
			start:      "2026-09-06T11:15:00Z",
			end:        "2026-09-06T11:30:00Z",
		},
		{
			name:       "midnight boundary",
			capturedAt: time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC),
			minutes:    15,
			start:      "2026-09-07T00:00:00Z",
			end:        "2026-09-07T00:15:00Z",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			window, err := canonicalFramePackageWindow(test.capturedAt, test.minutes)
			if err != nil {
				t.Fatal(err)
			}
			if got := window.start.Format(time.RFC3339); got != test.start {
				t.Fatalf("window start = %s, want %s", got, test.start)
			}
			if got := window.end.Format(time.RFC3339); got != test.end {
				t.Fatalf("window end = %s, want %s", got, test.end)
			}
		})
	}
}

func TestCanonicalFramePackageWindowRejectsInvalidInterval(t *testing.T) {
	for _, minutes := range []int{0, -1} {
		if _, err := canonicalFramePackageWindow(time.Now(), minutes); err == nil {
			t.Fatalf("interval %d was accepted", minutes)
		}
	}
}

func TestFramePackageObjectNameUsesCanonicalWindow(t *testing.T) {
	window, err := canonicalFramePackageWindow(
		time.Date(2026, 9, 6, 11, 7, 3, 456000000, time.UTC),
		15,
	)
	if err != nil {
		t.Fatal(err)
	}
	got := framePackageObjectName(1, 1, 83, window)
	want := "1/1/83/2026-09-06T11-00-00.000Z__2026-09-06T11-15-00.000Z.tar"
	if got != want {
		t.Fatalf("object name = %q, want %q", got, want)
	}
}

func TestFramePackageTarIsDeterministicAndChronological(t *testing.T) {
	cache := newTestFramePackageCache(t, 1)
	frames := []struct {
		at   time.Time
		data string
	}{
		{time.Date(2026, 9, 6, 11, 0, 30, 444999999, time.UTC), "third"},
		{time.Date(2026, 9, 6, 11, 0, 10, 111999999, time.UTC), "first"},
		{time.Date(2026, 9, 6, 11, 0, 10, 111999999, time.UTC), "second"},
	}
	for _, frame := range frames {
		if err := cache.addFrame(frame.at, []byte(frame.data)); err != nil {
			t.Fatal(err)
		}
	}
	cache.advance(time.Date(2026, 9, 6, 11, 1, 0, 0, time.UTC))
	upload, ok := cache.completedUpload()
	if !ok {
		t.Fatal("closed package is missing")
	}

	var first bytes.Buffer
	if err := writeFramePackageTar(&first, upload); err != nil {
		t.Fatal(err)
	}
	var second bytes.Buffer
	if err := writeFramePackageTar(&second, upload); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("two TAR builds produced different bytes")
	}

	reader := tar.NewReader(bytes.NewReader(first.Bytes()))
	wantNames := []string{
		"2026-09-06T11-00-10.111Z.jpg",
		"2026-09-06T11-00-10.111Z__000002.jpg",
		"2026-09-06T11-00-30.444Z.jpg",
	}
	wantPayloads := []string{"first", "second", "third"}
	for index := range wantNames {
		header, err := reader.Next()
		if err != nil {
			t.Fatalf("entry %d: %v", index, err)
		}
		if header.Name != wantNames[index] || header.Typeflag != tar.TypeReg {
			t.Fatalf("entry %d = %#v, want regular %q", index, header, wantNames[index])
		}
		if header.Mode != 0o600 || !header.ModTime.Equal(time.Unix(0, 0).UTC()) {
			t.Fatalf("entry %d has non-deterministic metadata: %#v", index, header)
		}
		payload, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if string(payload) != wantPayloads[index] {
			t.Fatalf("entry %d payload = %q, want %q", index, payload, wantPayloads[index])
		}
	}
	if header, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("unexpected extra TAR entry %#v, error %v", header, err)
	}
}

func TestFramePackageCacheCreatesNothingForEmptyWindow(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "device")
	cache, err := newFramePackageCache(directory, 15)
	if err != nil {
		t.Fatal(err)
	}
	cache.advance(time.Date(2026, 9, 6, 11, 15, 0, 0, time.UTC))
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty window created state: %v", entries)
	}
	if _, ok := cache.completedUpload(); ok {
		t.Fatal("empty window produced an upload")
	}
}

func TestFramePackageTwoWindowLifecycleDropsAWhenBCloses(t *testing.T) {
	cache := newTestFramePackageCache(t, 1)
	aTime := time.Date(2026, 9, 6, 11, 0, 10, 0, time.UTC)
	bTime := time.Date(2026, 9, 6, 11, 1, 10, 0, time.UTC)
	cTime := time.Date(2026, 9, 6, 11, 2, 10, 0, time.UTC)

	if err := cache.addFrame(aTime, []byte("A")); err != nil {
		t.Fatal(err)
	}
	cache.advance(time.Date(2026, 9, 6, 11, 1, 0, 0, time.UTC))
	a, ok := cache.completedUpload()
	if !ok {
		t.Fatal("A did not become completed")
	}
	if err := cache.addFrame(bTime, []byte("B")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.directory); err != nil {
		t.Fatalf("A was discarded before B closed: %v", err)
	}

	cache.advance(time.Date(2026, 9, 6, 11, 2, 0, 0, time.UTC))
	if _, err := os.Stat(a.directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("A survived B closure: %v", err)
	}
	b, ok := cache.completedUpload()
	if !ok || !b.window.start.Equal(bTime.Truncate(time.Minute)) {
		t.Fatalf("B was not promoted: %#v", b)
	}
	if err := cache.addFrame(cTime, []byte("C")); err != nil {
		t.Fatal(err)
	}

	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if cache.completed == nil || cache.current == nil {
		t.Fatal("B completed and C current were not both retained")
	}
	entries, err := os.ReadDir(cache.directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("retained package count = %d, want 2", len(entries))
	}
}

func TestFramePackageTwoWindowLifecycleKeepsBWhenASucceeds(t *testing.T) {
	cache := newTestFramePackageCache(t, 1)
	aTime := time.Date(2026, 9, 6, 11, 0, 10, 0, time.UTC)
	bTime := time.Date(2026, 9, 6, 11, 1, 10, 0, time.UTC)
	if err := cache.addFrame(aTime, []byte("A")); err != nil {
		t.Fatal(err)
	}
	cache.advance(time.Date(2026, 9, 6, 11, 1, 0, 0, time.UTC))
	a, _ := cache.completedUpload()
	if err := cache.addFrame(bTime, []byte("B")); err != nil {
		t.Fatal(err)
	}
	if !cache.markUploaded(a.id, time.Date(2026, 9, 6, 11, 1, 30, 0, time.UTC)) {
		t.Fatal("A success was not accepted")
	}
	cache.advance(time.Date(2026, 9, 6, 11, 2, 0, 0, time.UTC))
	b, ok := cache.completedUpload()
	if !ok || !b.window.start.Equal(bTime.Truncate(time.Minute)) {
		t.Fatalf("B was not retained after A success: %#v", b)
	}
}

func TestFramePackageCaptureContinuesWhileAUploads(t *testing.T) {
	cache := newTestFramePackageCache(t, 1)
	if err := cache.addFrame(time.Date(2026, 9, 6, 11, 0, 1, 0, time.UTC), []byte("A")); err != nil {
		t.Fatal(err)
	}
	cache.advance(time.Date(2026, 9, 6, 11, 1, 0, 0, time.UTC))
	a, _ := cache.completedUpload()
	uploadCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if !cache.beginUpload(a.id, cancel) {
		t.Fatal("A upload did not begin")
	}
	if err := cache.addFrame(time.Date(2026, 9, 6, 11, 1, 1, 0, time.UTC), []byte("B1")); err != nil {
		t.Fatal(err)
	}
	if err := cache.addFrame(time.Date(2026, 9, 6, 11, 1, 2, 0, time.UTC), []byte("B2")); err != nil {
		t.Fatal(err)
	}
	if uploadCtx.Err() != nil {
		t.Fatal("A upload was canceled before B closed")
	}
	cache.mutex.Lock()
	frames := len(cache.current.frames)
	cache.mutex.Unlock()
	if frames != 2 {
		t.Fatalf("B frame count = %d, want 2", frames)
	}
}

func TestFramePackageUploaderRetriesSameCompletedPackage(t *testing.T) {
	cache := newTestFramePackageCache(t, 1)
	now := time.Now().UTC()
	window, err := canonicalFramePackageWindow(now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.addFrame(now, []byte("frame")); err != nil {
		t.Fatal(err)
	}
	cache.advance(window.end)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	var (
		attempts int
		firstID  uint64
	)
	go runFramePackageUploader(
		ctx,
		cache,
		10*time.Millisecond,
		func(_ context.Context, upload framePackageUpload) (time.Time, error) {
			attempts++
			if attempts == 1 {
				firstID = upload.id
				return time.Time{}, errors.New("ambiguous upload")
			}
			if upload.id != firstID {
				t.Errorf("retry package id = %d, want %d", upload.id, firstID)
			}
			return time.Now().UTC(), nil
		},
		func(time.Time) {
			close(done)
			cancel()
		},
	)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("package retry did not complete")
	}
	if attempts != 2 {
		t.Fatalf("upload attempts = %d, want 2", attempts)
	}
}

func TestFramePackageUploaderCancelsAndDiscardsAtLifetimeBoundary(t *testing.T) {
	cache := newTestFramePackageCache(t, 1)
	now := time.Now().UTC()
	window, err := canonicalFramePackageWindow(now, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.addFrame(now, []byte("A")); err != nil {
		t.Fatal(err)
	}
	cache.advance(window.end)
	upload, ok := cache.completedUpload()
	if !ok {
		t.Fatal("A did not close")
	}
	cache.mutex.Lock()
	cache.completed.expiresAt = time.Now().UTC().Add(5 * time.Second)
	cache.mutex.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attemptStarted := make(chan struct{})
	attemptFinished := make(chan struct{})
	go runFramePackageUploader(
		ctx,
		cache,
		time.Second,
		func(uploadCtx context.Context, _ framePackageUpload) (time.Time, error) {
			close(attemptStarted)
			<-uploadCtx.Done()
			close(attemptFinished)
			return time.Time{}, uploadCtx.Err()
		},
		nil,
	)
	select {
	case <-attemptStarted:
	case <-time.After(time.Second):
		t.Fatal("package upload did not start")
	}
	boundary := time.Now().UTC()
	cache.mutex.Lock()
	cache.completed.expiresAt = boundary
	cache.mutex.Unlock()
	cache.advance(boundary)
	select {
	case <-attemptFinished:
	case <-time.After(time.Second):
		t.Fatal("expired upload did not honor cancellation")
	}
	deadline := time.Now().Add(time.Second)
	for {
		_, statErr := os.Stat(upload.directory)
		if errors.Is(statErr, os.ErrNotExist) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expired package was not discarded: %v", statErr)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFramePackageRetryDelayAllowsRetryWithinShortWindow(t *testing.T) {
	tests := []struct {
		minutes int
		want    time.Duration
	}{
		{1, 30 * time.Second},
		{2, time.Minute},
		{5, retryDelay},
		{15, retryDelay},
	}
	for _, test := range tests {
		if got := framePackageRetryDelay(test.minutes); got != test.want {
			t.Fatalf("retry delay for %d minutes = %v, want %v", test.minutes, got, test.want)
		}
	}
}

func TestResetFramePackageRootDiscardsOnlyPreviousPackageState(t *testing.T) {
	work := t.TempDir()
	helper := filepath.Join(work, "lifecycle-helper.exe")
	if err := os.WriteFile(helper, []byte("helper"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := resetFramePackageRoot(work)
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "1", "1", "83", "stale.jpg")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	restartedRoot, err := resetFramePackageRoot(work)
	if err != nil {
		t.Fatal(err)
	}
	if restartedRoot != root {
		t.Fatalf("restart root = %q, want %q", restartedRoot, root)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale package survived restart: %v", err)
	}
	if encoded, err := os.ReadFile(helper); err != nil || string(encoded) != "helper" {
		t.Fatalf("package reset touched lifecycle state: %q, %v", encoded, err)
	}
	if err := removeFramePackageRoot(work); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("package root survived shutdown: %v", err)
	}
	if _, err := os.Stat(helper); err != nil {
		t.Fatalf("package shutdown cleanup touched lifecycle state: %v", err)
	}
}

func TestWriteFramePackageTarRejectsEmptyPackage(t *testing.T) {
	if err := writeFramePackageTar(io.Discard, framePackageUpload{}); err == nil {
		t.Fatal("empty package produced a TAR")
	}
}

func newTestFramePackageCache(t *testing.T, intervalMinutes int) *framePackageCache {
	t.Helper()
	cache, err := newFramePackageCache(
		filepath.Join(t.TempDir(), "organisation", "site", "device"),
		intervalMinutes,
	)
	if err != nil {
		t.Fatal(err)
	}
	return cache
}
