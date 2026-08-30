package connect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	comparisonWidth       = 160
	comparisonHeight      = 90
	comparisonCellCount   = comparisonWidth * comparisonHeight
	comparisonBlockWidth  = frameWidth / comparisonWidth
	comparisonBlockHeight = frameHeight / comparisonHeight
	materialLumaDelta     = 12
	processWaitDelay      = 2 * time.Second
)

type frameCandidate struct {
	rgb        []byte
	observedAt time.Time
}

type frameEncoder func([]byte) ([]byte, error)
type frameUploader func(context.Context, []byte, time.Time) error

type streamLifecycle struct {
	started func(context.Context, time.Time)
	exited  func(context.Context, time.Time, *int)
}

type streamProcessor struct {
	changeThresholdPercent float64
	baseline               []byte
	encode                 frameEncoder
	upload                 frameUploader
	clock                  func() time.Time
	afterUpload            func(context.Context, time.Time, time.Time) error
	successfulUploads      int
}

func newStreamProcessor(thresholdPercent float64, encode frameEncoder, upload frameUploader) *streamProcessor {
	return &streamProcessor{
		changeThresholdPercent: thresholdPercent,
		encode:                 encode,
		upload:                 upload,
		clock:                  time.Now,
	}
}

func (processor *streamProcessor) process(ctx context.Context, candidate frameCandidate) (bool, error) {
	comparison, err := comparisonFrame(candidate.rgb)
	if err != nil {
		return false, err
	}
	if processor.baseline != nil && !materiallyChanged(processor.baseline, comparison, processor.changeThresholdPercent) {
		return false, nil
	}

	jpegData, err := processor.encode(candidate.rgb)
	if err != nil {
		return false, err
	}
	if err := processor.upload(ctx, jpegData, candidate.observedAt); err != nil {
		return false, err
	}

	processor.baseline = comparison
	processor.successfulUploads++
	completedAt := processor.clock().UTC()
	if processor.afterUpload != nil {
		if err := processor.afterUpload(ctx, candidate.observedAt, completedAt); err != nil {
			return true, err
		}
	}
	return true, nil
}

func comparisonFrame(rgb []byte) ([]byte, error) {
	if len(rgb) != rawFrameSize {
		return nil, fmt.Errorf("frame capture/decode failed: expected %d RGB24 bytes, received %d", rawFrameSize, len(rgb))
	}

	comparison := make([]byte, comparisonCellCount)
	pixelsPerCell := comparisonBlockWidth * comparisonBlockHeight
	for cellY := 0; cellY < comparisonHeight; cellY++ {
		for cellX := 0; cellX < comparisonWidth; cellX++ {
			lumaSum := 0
			for blockY := 0; blockY < comparisonBlockHeight; blockY++ {
				y := cellY*comparisonBlockHeight + blockY
				rowOffset := y * frameWidth * rgbBytesPerPixel
				for blockX := 0; blockX < comparisonBlockWidth; blockX++ {
					x := cellX*comparisonBlockWidth + blockX
					offset := rowOffset + x*rgbBytesPerPixel
					lumaSum += (77*int(rgb[offset]) + 150*int(rgb[offset+1]) + 29*int(rgb[offset+2]) + 128) >> 8
				}
			}
			comparison[cellY*comparisonWidth+cellX] = byte(lumaSum / pixelsPerCell)
		}
	}
	return comparison, nil
}

func materiallyChanged(baseline, candidate []byte, thresholdPercent float64) bool {
	if len(baseline) != comparisonCellCount || len(candidate) != comparisonCellCount {
		return true
	}

	changedCells := 0
	for index, baselineLuma := range baseline {
		delta := int(baselineLuma) - int(candidate[index])
		if delta < 0 {
			delta = -delta
		}
		if delta >= materialLumaDelta {
			changedCells++
		}
	}

	return float64(changedCells)*100/float64(comparisonCellCount) >= thresholdPercent
}

func ffmpegArguments(sourceURI string, fps int) []string {
	filter := "fps=" + strconv.Itoa(fps) + ",scale=1280:720:force_original_aspect_ratio=decrease,pad=1280:720:(ow-iw)/2:(oh-ih)/2:black,format=rgb24"
	return []string{
		"-hide_banner",
		"-loglevel", "error",
		"-nostats",
		"-nostdin",
		"-rtsp_transport", "tcp",
		"-i", sourceURI,
		"-map", "0:v:0",
		"-an",
		"-sn",
		"-dn",
		"-vf", filter,
		"-f", "rawvideo",
		"-pix_fmt", "rgb24",
		"pipe:1",
	}
}

func streamRTSP(
	ctx context.Context,
	source SourceConfig,
	fps int,
	clock func() time.Time,
	lifecycle streamLifecycle,
	process func(frameCandidate) error,
) error {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return errors.New("frame capture/decode failed: ffmpeg not found on PATH")
	}

	sourceURI, err := authenticatedRTSPURI(source)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, ffmpegPath, ffmpegArguments(sourceURI, fps)...)
	return consumeCommandStream(ctx, cmd, clock, lifecycle, process)
}

func consumeCommandStream(
	ctx context.Context,
	cmd *exec.Cmd,
	clock func() time.Time,
	lifecycle streamLifecycle,
	process func(frameCandidate) error,
) error {
	stderr := newCappedBuffer(ffmpegErrorLimit)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errors.New("frame capture/decode failed: unable to open FFmpeg output")
	}
	cmd.Stderr = stderr
	cmd.WaitDelay = processWaitDelay

	if err := cmd.Start(); err != nil {
		return errors.New("frame capture/decode failed: FFmpeg could not start")
	}
	if lifecycle.started != nil {
		lifecycle.started(ctx, clock().UTC())
	}

	var processingErr error
	readErr := readRGBFrames(ctx, stdout, clock, func(candidate frameCandidate) error {
		processingErr = process(candidate)
		return processingErr
	})

	// readRGBFrames only returns when the stream, context, or processing stops.
	// Kill is harmless for an already-exited child; Wait always reaps it.
	_ = cmd.Process.Kill()
	_ = stdout.Close()
	waitErr := cmd.Wait()
	var exitCode *int
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		exitCode = &code
	}
	if lifecycle.exited != nil {
		lifecycle.exited(ctx, clock().UTC(), exitCode)
	}

	if processingErr != nil {
		return processingErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if isIncompleteFrameError(readErr) {
		return readErr
	}
	if waitErr != nil {
		return classifyFFmpegFailure(stderr.String())
	}
	if readErr != nil {
		return readErr
	}
	return errors.New("RTSP stream ended unexpectedly")
}

type incompleteFrameError struct {
	received int
}

func (err incompleteFrameError) Error() string {
	return fmt.Sprintf("frame capture/decode failed: incomplete RGB24 frame: expected %d bytes, received %d", rawFrameSize, err.received)
}

func isIncompleteFrameError(err error) bool {
	var incomplete incompleteFrameError
	return errors.As(err, &incomplete)
}

func readRGBFrames(ctx context.Context, reader io.Reader, clock func() time.Time, process func(frameCandidate) error) error {
	frame := make([]byte, rawFrameSize)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		bytesRead, err := io.ReadFull(reader, frame)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if bytesRead > 0 {
				return incompleteFrameError{received: bytesRead}
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return errors.New("RTSP stream ended unexpectedly")
			}
			return errors.New("frame capture/decode failed: unable to read FFmpeg output")
		}

		candidate := frameCandidate{rgb: frame, observedAt: clock().UTC()}
		if err := process(candidate); err != nil {
			return err
		}
	}
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
