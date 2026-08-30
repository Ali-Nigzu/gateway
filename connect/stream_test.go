package connect

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

var errStopReading = errors.New("stop reading")

func TestFFmpegArgumentsConfigurePersistentThreeFPSStream(t *testing.T) {
	args := strings.Join(ffmpegArguments("rtsp://camera.example/live", 3), " ")
	for _, required := range []string{
		"-nostdin",
		"-rtsp_transport tcp",
		"fps=3,scale=1280:720:force_original_aspect_ratio=decrease",
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
	if strings.Contains(args, "-frames:v") {
		t.Fatalf("FFmpeg arguments still limit the stream to one frame: %s", args)
	}
}

func TestReadRGBFramesContinuouslyReadsFixedSizeFrames(t *testing.T) {
	first := solidRGBFrame(10, 20, 30)
	second := solidRGBFrame(40, 50, 60)
	stream := bytes.NewReader(append(first, second...))
	observed := make([]byte, 0, 2)

	err := readRGBFrames(context.Background(), stream, time.Now, func(candidate frameCandidate) error {
		observed = append(observed, candidate.rgb[0])
		if len(observed) == 2 {
			return errStopReading
		}
		return nil
	})
	if !errors.Is(err, errStopReading) {
		t.Fatalf("readRGBFrames() error = %v, want stop sentinel", err)
	}
	if !bytes.Equal(observed, []byte{10, 40}) {
		t.Fatalf("observed frame starts = %v", observed)
	}
}

func TestReadRGBFramesRejectsIncompleteFrame(t *testing.T) {
	stream := bytes.NewReader(make([]byte, rawFrameSize+137))
	completeFrames := 0
	err := readRGBFrames(context.Background(), stream, time.Now, func(frameCandidate) error {
		completeFrames++
		return nil
	})
	if completeFrames != 1 {
		t.Fatalf("complete frame count = %d, want 1", completeFrames)
	}
	var incomplete incompleteFrameError
	if !errors.As(err, &incomplete) || incomplete.received != 137 {
		t.Fatalf("readRGBFrames() error = %v, want 137-byte incomplete frame", err)
	}
}

func TestFirstFrameAlwaysAccepted(t *testing.T) {
	processor, encoded, uploaded := recordingProcessor(t, nil)
	accepted, err := processor.process(context.Background(), frameCandidate{
		rgb:        solidRGBFrame(0, 0, 0),
		observedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("process() error = %v", err)
	}
	if !accepted || *encoded != 1 || *uploaded != 1 {
		t.Fatalf("accepted = %v, encoded = %d, uploaded = %d", accepted, *encoded, *uploaded)
	}
}

func TestIdenticalFrameAndSmallNoiseAreSuppressedBeforeJPEG(t *testing.T) {
	processor, encoded, uploaded := recordingProcessor(t, nil)
	baseline := solidRGBFrame(80, 80, 80)
	if accepted, err := processor.process(context.Background(), frameCandidate{rgb: baseline, observedAt: time.Now()}); err != nil || !accepted {
		t.Fatalf("first process() = accepted %v, error %v", accepted, err)
	}

	identicalAccepted, err := processor.process(context.Background(), frameCandidate{
		rgb:        append([]byte(nil), baseline...),
		observedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("identical process() error = %v", err)
	}
	noiseAccepted, err := processor.process(context.Background(), frameCandidate{
		rgb:        solidRGBFrame(85, 85, 85),
		observedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("noise process() error = %v", err)
	}
	if identicalAccepted || noiseAccepted {
		t.Fatalf("near-identical frames accepted: identical=%v noise=%v", identicalAccepted, noiseAccepted)
	}
	if *encoded != 1 || *uploaded != 1 {
		t.Fatalf("rejected candidates reached JPEG/upload: encoded=%d uploaded=%d", *encoded, *uploaded)
	}
}

func TestMeaningfulLocalMovementIsAccepted(t *testing.T) {
	processor, _, uploaded := recordingProcessor(t, nil)
	baseline := solidRGBFrame(0, 0, 0)
	movement := append([]byte(nil), baseline...)
	paintComparisonCells(movement, 0, 72, 255)

	if accepted, err := processor.process(context.Background(), frameCandidate{rgb: baseline, observedAt: time.Now()}); err != nil || !accepted {
		t.Fatalf("baseline process() = accepted %v, error %v", accepted, err)
	}
	accepted, err := processor.process(context.Background(), frameCandidate{rgb: movement, observedAt: time.Now()})
	if err != nil {
		t.Fatalf("movement process() error = %v", err)
	}
	if !accepted || *uploaded != 2 {
		t.Fatalf("movement accepted = %v, uploads = %d", accepted, *uploaded)
	}
}

func TestComparisonRemainsAgainstLastSuccessfulUpload(t *testing.T) {
	processor, _, uploaded := recordingProcessor(t, nil)
	baseline := solidRGBFrame(0, 0, 0)
	skipped := append([]byte(nil), baseline...)
	paintComparisonCells(skipped, 0, 40, 255)
	changed := append([]byte(nil), baseline...)
	paintComparisonCells(changed, 0, 80, 255)

	accepted, err := processor.process(context.Background(), frameCandidate{rgb: baseline, observedAt: time.Now()})
	if err != nil || !accepted {
		t.Fatalf("baseline process() = accepted %v, error %v", accepted, err)
	}
	wantBaseline := append([]byte(nil), processor.baseline...)

	accepted, err = processor.process(context.Background(), frameCandidate{rgb: skipped, observedAt: time.Now()})
	if err != nil || accepted {
		t.Fatalf("small change process() = accepted %v, error %v", accepted, err)
	}
	if !bytes.Equal(processor.baseline, wantBaseline) {
		t.Fatal("skipped frame changed the baseline")
	}

	accepted, err = processor.process(context.Background(), frameCandidate{rgb: changed, observedAt: time.Now()})
	if err != nil {
		t.Fatalf("larger change process() error = %v", err)
	}
	if !accepted || *uploaded != 2 {
		t.Fatalf("change relative to uploaded baseline accepted = %v, uploads = %d", accepted, *uploaded)
	}
}

func TestFailedUploadDoesNotChangeBaseline(t *testing.T) {
	uploadFailure := errors.New("upload failed")
	uploadAttempts := 0
	afterUploadCalls := 0
	processor := newStreamProcessor(
		0.5,
		func([]byte) ([]byte, error) { return []byte("jpeg"), nil },
		func(context.Context, []byte, time.Time) error {
			uploadAttempts++
			if uploadAttempts == 2 {
				return uploadFailure
			}
			return nil
		},
	)
	processor.afterUpload = func(context.Context, time.Time, time.Time) error {
		afterUploadCalls++
		return nil
	}
	baselineFrame := solidRGBFrame(0, 0, 0)
	failedFrame := solidRGBFrame(255, 255, 255)
	if accepted, err := processor.process(context.Background(), frameCandidate{rgb: baselineFrame, observedAt: time.Now()}); err != nil || !accepted {
		t.Fatalf("baseline process() = accepted %v, error %v", accepted, err)
	}
	wantBaseline := append([]byte(nil), processor.baseline...)

	accepted, err := processor.process(context.Background(), frameCandidate{rgb: failedFrame, observedAt: time.Now()})
	if accepted || !errors.Is(err, uploadFailure) {
		t.Fatalf("failed upload process() = accepted %v, error %v", accepted, err)
	}
	if !bytes.Equal(processor.baseline, wantBaseline) {
		t.Fatal("failed upload changed the baseline")
	}
	if processor.successfulUploads != 1 {
		t.Fatalf("successful upload count = %d, want 1", processor.successfulUploads)
	}
	if afterUploadCalls != 1 {
		t.Fatalf("database upload callback calls = %d, want only the successful upload", afterUploadCalls)
	}
}

func TestSuccessfulUploadAdvancesBaselineBeforeRuntimeFactCallback(t *testing.T) {
	capturedAt := time.Date(2026, 8, 30, 14, 3, 27, 123456789, time.UTC)
	completedAt := capturedAt.Add(420 * time.Millisecond)
	factFailure := errors.New("database runtime fact update failed")
	uploadCompleted := false
	processor := newStreamProcessor(
		0.5,
		func([]byte) ([]byte, error) { return []byte("jpeg"), nil },
		func(context.Context, []byte, time.Time) error {
			uploadCompleted = true
			return nil
		},
	)
	processor.clock = func() time.Time { return completedAt }
	processor.afterUpload = func(_ context.Context, captured, completed time.Time) error {
		if !uploadCompleted || processor.baseline == nil || processor.successfulUploads != 1 {
			t.Fatal("runtime fact callback ran before GCS success and baseline advancement")
		}
		if !captured.Equal(capturedAt) || !completed.Equal(completedAt) {
			t.Fatalf("callback timestamps = %v, %v", captured, completed)
		}
		return factFailure
	}

	accepted, err := processor.process(context.Background(), frameCandidate{
		rgb: solidRGBFrame(12, 34, 56), observedAt: capturedAt,
	})
	if !accepted || !errors.Is(err, factFailure) {
		t.Fatalf("process() = accepted %v, error %v", accepted, err)
	}
	if processor.baseline == nil || processor.successfulUploads != 1 {
		t.Fatal("successful GCS object was rolled back after database failure")
	}
}

func TestCandidateTimestampComesFromObservationClock(t *testing.T) {
	want := time.Date(2026, 8, 30, 14, 3, 27, 123456789, time.FixedZone("local", -4*60*60))
	var got time.Time
	processor := newStreamProcessor(
		0.5,
		func([]byte) ([]byte, error) { return []byte("jpeg"), nil },
		func(_ context.Context, _ []byte, observedAt time.Time) error {
			got = observedAt
			return nil
		},
	)

	err := readRGBFrames(
		context.Background(),
		bytes.NewReader(solidRGBFrame(0, 0, 0)),
		func() time.Time { return want },
		func(candidate frameCandidate) error {
			accepted, err := processor.process(context.Background(), candidate)
			if err != nil {
				return err
			}
			if !accepted {
				t.Fatal("first frame was not accepted")
			}
			return errStopReading
		},
	)
	if !errors.Is(err, errStopReading) {
		t.Fatalf("readRGBFrames() error = %v, want stop sentinel", err)
	}
	if !got.Equal(want) || got.Location() != time.UTC {
		t.Fatalf("upload timestamp = %v (%v), want UTC %v", got, got.Location(), want.UTC())
	}
}

func TestReadRGBFramesStopsCleanlyOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	err := readRGBFrames(ctx, bytes.NewReader(solidRGBFrame(0, 0, 0)), time.Now, func(frameCandidate) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("readRGBFrames() error = %v, want context.Canceled", err)
	}
}

func TestConsumeCommandStreamCancelsAndReapsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStreamHelperProcess$")
	cmd.Env = append(os.Environ(), "CAMOS_STREAM_HELPER=1")
	started := 0
	exited := 0

	err := consumeCommandStream(ctx, cmd, time.Now, streamLifecycle{
		started: func(context.Context, time.Time) { started++ },
		exited: func(context.Context, time.Time, *int) {
			exited++
		},
	}, func(frameCandidate) error {
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("consumeCommandStream() error = %v, want context.Canceled", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("helper process was not reaped: state=%v", cmd.ProcessState)
	}
	if started != 1 || exited != 1 {
		t.Fatalf("FFmpeg lifecycle callbacks: started=%d exited=%d", started, exited)
	}
}

func TestStreamHelperProcess(t *testing.T) {
	if os.Getenv("CAMOS_STREAM_HELPER") != "1" {
		return
	}
	if _, err := os.Stdout.Write(make([]byte, rawFrameSize)); err != nil {
		os.Exit(2)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func recordingProcessor(t *testing.T, uploadError error) (*streamProcessor, *int, *int) {
	t.Helper()
	encoded := 0
	uploaded := 0
	processor := newStreamProcessor(
		0.5,
		func([]byte) ([]byte, error) {
			encoded++
			return []byte("jpeg"), nil
		},
		func(context.Context, []byte, time.Time) error {
			uploaded++
			return uploadError
		},
	)
	return processor, &encoded, &uploaded
}

func solidRGBFrame(red, green, blue byte) []byte {
	frame := make([]byte, rawFrameSize)
	for offset := 0; offset < len(frame); offset += rgbBytesPerPixel {
		frame[offset] = red
		frame[offset+1] = green
		frame[offset+2] = blue
	}
	return frame
}

func paintComparisonCells(frame []byte, start, count int, value byte) {
	for cell := start; cell < start+count; cell++ {
		cellX := cell % comparisonWidth
		cellY := cell / comparisonWidth
		for blockY := 0; blockY < comparisonBlockHeight; blockY++ {
			y := cellY*comparisonBlockHeight + blockY
			for blockX := 0; blockX < comparisonBlockWidth; blockX++ {
				x := cellX*comparisonBlockWidth + blockX
				offset := (y*frameWidth + x) * rgbBytesPerPixel
				frame[offset] = value
				frame[offset+1] = value
				frame[offset+2] = value
			}
		}
	}
}
