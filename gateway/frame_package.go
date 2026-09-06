package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	framePackagesDirectoryName = "frame-packages"
	framePackageTimeLayout     = "2006-01-02T15-04-05.000Z"
)

type framePackageWindow struct {
	start time.Time
	end   time.Time
}

// canonicalFramePackageWindow returns the globally aligned UTC window that
// contains capturedAt. Alignment is relative to the Unix epoch, never process
// start time.
func canonicalFramePackageWindow(
	capturedAt time.Time,
	intervalMinutes int,
) (framePackageWindow, error) {
	intervalSeconds, err := framePackageIntervalSeconds(intervalMinutes)
	if err != nil {
		return framePackageWindow{}, err
	}

	timestamp := capturedAt.Unix()
	quotient := timestamp / intervalSeconds
	if timestamp%intervalSeconds < 0 {
		quotient--
	}
	if quotient > math.MaxInt64/intervalSeconds ||
		quotient < math.MinInt64/intervalSeconds {
		return framePackageWindow{}, errors.New("frame package timestamp is out of range")
	}
	startUnix := quotient * intervalSeconds
	if startUnix > math.MaxInt64-intervalSeconds {
		return framePackageWindow{}, errors.New("frame package timestamp is out of range")
	}

	return framePackageWindow{
		start: time.Unix(startUnix, 0).UTC(),
		end:   time.Unix(startUnix+intervalSeconds, 0).UTC(),
	}, nil
}

func framePackageIntervalSeconds(intervalMinutes int) (int64, error) {
	if intervalMinutes <= 0 ||
		int64(intervalMinutes) > math.MaxInt64/int64(time.Minute) {
		return 0, errors.New("frame package interval must be positive")
	}
	return int64(intervalMinutes) * 60, nil
}

func framePackageObjectName(
	organisationID int64,
	siteID int64,
	deviceID int64,
	window framePackageWindow,
) string {
	return strconv.FormatInt(organisationID, 10) + "/" +
		strconv.FormatInt(siteID, 10) + "/" +
		strconv.FormatInt(deviceID, 10) + "/" +
		window.start.UTC().Format(framePackageTimeLayout) + "__" +
		window.end.UTC().Format(framePackageTimeLayout) + ".tar"
}

func framePackageDeviceDirectory(
	root string,
	organisationID int64,
	siteID int64,
	deviceID int64,
) string {
	return filepath.Join(
		root,
		strconv.FormatInt(organisationID, 10),
		strconv.FormatInt(siteID, 10),
		strconv.FormatInt(deviceID, 10),
	)
}

// resetFramePackageRoot discards all package state from an earlier execution
// and returns a new empty package root. The work directory itself must already
// have been prepared with the platform's identity-directory protections.
func resetFramePackageRoot(workDirectory string) (string, error) {
	root, err := checkedFramePackageRoot(workDirectory)
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(root); err != nil {
		return "", errors.New("stale frame package cleanup failed")
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		return "", errors.New("frame package root creation failed")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.Remove(root)
		return "", errors.New("frame package root permissions failed")
	}
	return root, nil
}

func removeFramePackageRoot(workDirectory string) error {
	root, err := checkedFramePackageRoot(workDirectory)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(root); err != nil {
		return errors.New("frame package cleanup failed")
	}
	return nil
}

func checkedFramePackageRoot(workDirectory string) (string, error) {
	if strings.TrimSpace(workDirectory) == "" {
		return "", errors.New("Gateway work directory is invalid")
	}
	absoluteWork, err := filepath.Abs(workDirectory)
	if err != nil {
		return "", errors.New("Gateway work directory is invalid")
	}
	info, err := os.Lstat(absoluteWork)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Gateway work directory is invalid")
	}
	root := filepath.Join(absoluteWork, framePackagesDirectoryName)
	relative, err := filepath.Rel(absoluteWork, root)
	if err != nil || relative != framePackagesDirectoryName {
		return "", errors.New("frame package root is invalid")
	}
	return root, nil
}

type framePackageFrame struct {
	capturedAt time.Time
	sequence   uint64
	path       string
	size       int64
}

type framePackageState struct {
	id        uint64
	window    framePackageWindow
	directory string
	frames    []framePackageFrame
	expiresAt time.Time
}

type framePackageUpload struct {
	id        uint64
	window    framePackageWindow
	directory string
	frames    []framePackageFrame
	expiresAt time.Time
}

type framePackageCache struct {
	mutex           sync.Mutex
	directory       string
	intervalMinutes int
	interval        time.Duration
	nextPackageID   uint64
	current         *framePackageState
	completed       *framePackageState
	uploadingID     uint64
	cancelUpload    context.CancelFunc
	wake            chan struct{}
}

// newFramePackageCache creates an empty cache for one device. Callers own
// removal of the enclosing frame-packages tree at process startup and shutdown;
// this type deliberately performs no recovery or enumeration of prior state.
func newFramePackageCache(
	directory string,
	intervalMinutes int,
) (*framePackageCache, error) {
	_, err := framePackageIntervalSeconds(intervalMinutes)
	if err != nil {
		return nil, err
	}
	absoluteDirectory, err := filepath.Abs(strings.TrimSpace(directory))
	if err != nil || strings.TrimSpace(directory) == "" {
		return nil, errors.New("frame package directory is invalid")
	}
	if err := os.MkdirAll(absoluteDirectory, 0o700); err != nil {
		return nil, errors.New("frame package directory creation failed")
	}
	info, err := os.Lstat(absoluteDirectory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("frame package directory is invalid")
	}
	if err := os.Chmod(absoluteDirectory, 0o700); err != nil {
		return nil, errors.New("frame package directory permissions failed")
	}

	return &framePackageCache{
		directory:       absoluteDirectory,
		intervalMinutes: intervalMinutes,
		interval:        time.Duration(intervalMinutes) * time.Minute,
		wake:            make(chan struct{}, 1),
	}, nil
}

// addFrame stages one already-accepted JPEG. It never waits for cloud
// upload. If a capture timestamp advances the cache into a new window, stale
// completed state is discarded before the new live window is opened.
func (cache *framePackageCache) addFrame(
	capturedAt time.Time,
	jpegBytes []byte,
) error {
	window, err := canonicalFramePackageWindow(
		capturedAt,
		cache.intervalMinutes,
	)
	if err != nil {
		return err
	}
	capturedAt = capturedAt.UTC()

	cache.mutex.Lock()
	discarded, changed := cache.advanceLocked(capturedAt)
	if cache.current != nil && window.start.Before(cache.current.window.start) {
		cache.mutex.Unlock()
		removeFramePackages(discarded)
		if changed {
			cache.signal()
		}
		return nil
	}
	if cache.current != nil && !window.start.Equal(cache.current.window.start) {
		cache.mutex.Unlock()
		removeFramePackages(discarded)
		if changed {
			cache.signal()
		}
		return errors.New("frame package window transition failed")
	}

	created := false
	if cache.current == nil {
		state, createErr := cache.createPackageLocked(window)
		if createErr != nil {
			cache.mutex.Unlock()
			removeFramePackages(discarded)
			if changed {
				cache.signal()
			}
			return createErr
		}
		cache.current = state
		created = true
		changed = true
	}

	writeErr := cache.writeFrameLocked(cache.current, capturedAt, jpegBytes)
	if writeErr != nil && created {
		discarded = append(discarded, cache.current)
		cache.current = nil
	}
	cache.mutex.Unlock()

	removeFramePackages(discarded)
	if changed {
		cache.signal()
	}
	return writeErr
}

func (cache *framePackageCache) createPackageLocked(
	window framePackageWindow,
) (*framePackageState, error) {
	if cache.nextPackageID == math.MaxUint64 {
		return nil, errors.New("frame package sequence exhausted")
	}
	cache.nextPackageID++
	directory := filepath.Join(
		cache.directory,
		window.start.Format(framePackageTimeLayout)+"__"+
			window.end.Format(framePackageTimeLayout),
	)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, errors.New("frame package creation failed")
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.Remove(directory)
		return nil, errors.New("frame package permissions failed")
	}
	return &framePackageState{
		id:        cache.nextPackageID,
		window:    window,
		directory: directory,
	}, nil
}

func (cache *framePackageCache) writeFrameLocked(
	state *framePackageState,
	capturedAt time.Time,
	jpegBytes []byte,
) error {
	sequence := uint64(len(state.frames)) + 1

	temporary, err := os.CreateTemp(state.directory, ".frame-")
	if err != nil {
		return errors.New("frame package temporary file creation failed")
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		_ = temporary.Close()
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return errors.New("frame package file permissions failed")
	}
	written, err := temporary.Write(jpegBytes)
	if err != nil || written != len(jpegBytes) {
		return errors.New("frame package frame write failed")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("frame package frame close failed")
	}

	finalPath := filepath.Join(state.directory, fmt.Sprintf("%020d.jpg", sequence))
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return errors.New("frame package frame publication failed")
	}
	keepTemporary = false
	state.frames = append(state.frames, framePackageFrame{
		capturedAt: capturedAt,
		sequence:   sequence,
		path:       finalPath,
		size:       int64(len(jpegBytes)),
	})
	return nil
}

// advance closes elapsed windows and applies the two-window lifetime policy.
// It is also used by the uploader's wall-clock timer so closure does not depend
// on another accepted frame arriving.
func (cache *framePackageCache) advance(now time.Time) {
	cache.mutex.Lock()
	discarded, changed := cache.advanceLocked(now.UTC())
	cache.mutex.Unlock()
	removeFramePackages(discarded)
	if changed {
		cache.signal()
	}
}

func (cache *framePackageCache) advanceLocked(
	now time.Time,
) (discarded []*framePackageState, changed bool) {
	discardCompleted := func() {
		if cache.completed == nil {
			return
		}
		if cache.uploadingID == cache.completed.id && cache.cancelUpload != nil {
			cache.cancelUpload()
		}
		discarded = append(discarded, cache.completed)
		cache.completed = nil
		changed = true
	}

	if cache.completed != nil && !now.Before(cache.completed.expiresAt) {
		discardCompleted()
	}
	if cache.current != nil && !now.Before(cache.current.window.end) {
		// A completed package is never retained when the current package closes.
		// Ordinarily it expired at this exact boundary; this explicit replacement
		// also keeps the invariant intact after clock jumps.
		discardCompleted()
		cache.current.expiresAt = cache.current.window.end.Add(cache.interval)
		cache.completed = cache.current
		cache.current = nil
		changed = true
	}
	if cache.completed != nil && !now.Before(cache.completed.expiresAt) {
		discardCompleted()
	}
	return discarded, changed
}

func (cache *framePackageCache) completedUpload() (framePackageUpload, bool) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if cache.completed == nil {
		return framePackageUpload{}, false
	}
	return snapshotFramePackage(cache.completed), true
}

func snapshotFramePackage(state *framePackageState) framePackageUpload {
	return framePackageUpload{
		id:        state.id,
		window:    state.window,
		directory: state.directory,
		frames:    append([]framePackageFrame(nil), state.frames...),
		expiresAt: state.expiresAt,
	}
}

func (cache *framePackageCache) beginUpload(
	id uint64,
	cancel context.CancelFunc,
) bool {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	if cache.completed == nil || cache.completed.id != id {
		return false
	}
	cache.uploadingID = id
	cache.cancelUpload = cancel
	return true
}

func (cache *framePackageCache) endUpload(id uint64) {
	cache.mutex.Lock()
	if cache.uploadingID == id {
		cache.uploadingID = 0
		cache.cancelUpload = nil
	}
	cache.mutex.Unlock()
}

func (cache *framePackageCache) retains(id uint64) bool {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()
	return cache.current != nil && cache.current.id == id ||
		cache.completed != nil && cache.completed.id == id
}

func (cache *framePackageCache) markUploaded(id uint64, now time.Time) bool {
	cache.mutex.Lock()
	discarded, changed := cache.advanceLocked(now.UTC())
	if cache.completed == nil || cache.completed.id != id {
		cache.mutex.Unlock()
		removeFramePackages(discarded)
		if changed {
			cache.signal()
		}
		return false
	}
	uploaded := cache.completed
	cache.completed = nil
	cache.mutex.Unlock()

	removeFramePackages(append(discarded, uploaded))
	cache.signal()
	return true
}

func (cache *framePackageCache) nextEvent(
	retryID uint64,
	retryAt time.Time,
) (time.Time, bool) {
	cache.mutex.Lock()
	defer cache.mutex.Unlock()

	var deadline time.Time
	choose := func(candidate time.Time) {
		if !candidate.IsZero() && (deadline.IsZero() || candidate.Before(deadline)) {
			deadline = candidate
		}
	}
	if cache.current != nil {
		choose(cache.current.window.end)
	}
	if cache.completed != nil {
		choose(cache.completed.expiresAt)
		if retryID == cache.completed.id {
			choose(retryAt)
		} else {
			choose(time.Now().UTC())
		}
	}
	return deadline, !deadline.IsZero()
}

func (cache *framePackageCache) signal() {
	select {
	case cache.wake <- struct{}{}:
	default:
	}
}

func removeFramePackages(packages []*framePackageState) {
	for _, state := range packages {
		if state != nil {
			_ = os.RemoveAll(state.directory)
		}
	}
}

type framePackageUploadFunc func(
	context.Context,
	framePackageUpload,
) (time.Time, error)

// runFramePackageUploader closes current windows on wall-clock boundaries and
// retries one completed package without ever delaying collection of newer data.
// upload must honor its context; the context deadline is the exact instant at
// which the following canonical window closes and this package becomes stale.
func runFramePackageUploader(
	ctx context.Context,
	cache *framePackageCache,
	retryInterval time.Duration,
	upload framePackageUploadFunc,
	observeUpload func(time.Time),
) {
	if cache == nil || upload == nil {
		return
	}
	if retryInterval <= 0 {
		retryInterval = time.Second
	}

	var (
		retryID uint64
		retryAt time.Time
	)
	for {
		if ctx.Err() != nil {
			return
		}
		now := time.Now().UTC()
		cache.advance(now)
		completed, ok := cache.completedUpload()
		if ok && (retryID != completed.id || !now.Before(retryAt)) {
			attemptCtx, cancel := context.WithDeadline(ctx, completed.expiresAt)
			if !cache.beginUpload(completed.id, cancel) {
				cancel()
				continue
			}
			completedAt, err := upload(attemptCtx, completed)
			cache.endUpload(completed.id)
			cancel()
			finishedAt := time.Now().UTC()
			if err == nil && cache.markUploaded(completed.id, finishedAt) {
				retryID = 0
				retryAt = time.Time{}
				if observeUpload != nil {
					observeUpload(completedAt)
				}
				continue
			}
			cache.advance(finishedAt)
			// The capture goroutine may have expired this package while the
			// upload still had a source file open. Retry exact-directory cleanup
			// after upload returns so Windows cannot leave stale cache behind.
			if !cache.retains(completed.id) {
				removeFramePackages([]*framePackageState{{
					id:        completed.id,
					directory: completed.directory,
				}})
			}
			retryID = completed.id
			retryAt = finishedAt.Add(retryInterval)
			continue
		}

		deadline, hasDeadline := cache.nextEvent(retryID, retryAt)
		if !hasDeadline {
			select {
			case <-ctx.Done():
				return
			case <-cache.wake:
				continue
			}
		}
		delay := time.Until(deadline)
		if delay <= 0 {
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-cache.wake:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
}

func writeFramePackageTar(writer io.Writer, upload framePackageUpload) error {
	if writer == nil {
		return errors.New("frame package TAR destination is unavailable")
	}
	if len(upload.frames) == 0 {
		return errors.New("frame package contains no frames")
	}
	frames := append([]framePackageFrame(nil), upload.frames...)
	sort.Slice(frames, func(first, second int) bool {
		if frames[first].capturedAt.Equal(frames[second].capturedAt) {
			return frames[first].sequence < frames[second].sequence
		}
		return frames[first].capturedAt.Before(frames[second].capturedAt)
	})

	tarWriter := tar.NewWriter(writer)
	for _, frame := range frames {
		if err := writeFramePackageEntry(tarWriter, frame); err != nil {
			return err
		}
	}
	if err := tarWriter.Close(); err != nil {
		return fmt.Errorf("frame package TAR finalization failed: %w", err)
	}
	return nil
}

func writeFramePackageEntry(writer *tar.Writer, frame framePackageFrame) error {
	file, err := os.Open(frame.path)
	if err != nil {
		return errors.New("frame package frame is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() != frame.size {
		return errors.New("frame package frame is invalid")
	}

	header := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     frame.capturedAt.UTC().Format(framePackageTimeLayout) + ".jpg",
		Mode:     0o600,
		Size:     frame.size,
		ModTime:  time.Unix(0, 0).UTC(),
		Format:   tar.FormatUSTAR,
	}
	if err := writer.WriteHeader(header); err != nil {
		return fmt.Errorf("frame package TAR header write failed: %w", err)
	}
	written, err := io.Copy(writer, file)
	if err != nil {
		return fmt.Errorf("frame package TAR frame write failed: %w", err)
	}
	if written != frame.size {
		return errors.New("frame package TAR frame write failed")
	}
	return nil
}
