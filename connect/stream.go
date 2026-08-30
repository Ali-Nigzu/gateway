package connect

import (
	"context"
	"io"
	"os/exec"
	"strconv"
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

type streamProcessor struct {
	changeThresholdPercent float64
	baseline               []byte
	uploader               *gcsUploader
	observer               *deviceObserver
}

func newStreamProcessor(
	thresholdPercent float64,
	uploader *gcsUploader,
	observer *deviceObserver,
) *streamProcessor {
	return &streamProcessor{
		changeThresholdPercent: thresholdPercent,
		uploader:               uploader,
		observer:               observer,
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

	jpegData, err := encodeRGBFrame(candidate.rgb)
	if err != nil {
		return false, err
	}
	if err := processor.uploader.upload(ctx, jpegData, candidate.observedAt); err != nil {
		return false, err
	}

	// The image is durably uploaded before the baseline advances or database
	// facts are updated. A failed database update cannot cause a second object
	// write for the same captured frame.
	processor.baseline = comparison
	completedAt := time.Now().UTC()
	if err := processor.observer.uploaded(ctx, candidate.observedAt, completedAt); err != nil {
		return true, err
	}
	return true, nil
}

func comparisonFrame(rgb []byte) ([]byte, error) {
	if len(rgb) != rawFrameSize {
		return nil, deviceFailure{stage: failureStageStream}
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
	source sourceConfig,
	fps int,
	process func(frameCandidate) error,
) error {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return deviceFailure{stage: failureStageStream}
	}

	sourceURI, err := authenticatedRTSPURI(source)
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, ffmpegPath, ffmpegArguments(sourceURI, fps)...)
	return consumeCommandStream(ctx, cmd, process)
}

func consumeCommandStream(
	ctx context.Context,
	cmd *exec.Cmd,
	process func(frameCandidate) error,
) error {
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return deviceFailure{stage: failureStageStream}
	}
	cmd.Stderr = io.Discard
	cmd.WaitDelay = processWaitDelay

	if err := cmd.Start(); err != nil {
		return deviceFailure{stage: failureStageStream}
	}

	readErr := readRGBFrames(ctx, stdout, process)

	// readRGBFrames only returns when the stream, context, or processing stops.
	// Kill is harmless for an already-exited child; Wait always reaps it.
	_ = cmd.Process.Kill()
	_ = stdout.Close()
	_ = cmd.Wait()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	return readErr
}

func readRGBFrames(ctx context.Context, reader io.Reader, process func(frameCandidate) error) error {
	frame := make([]byte, rawFrameSize)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		_, err := io.ReadFull(reader, frame)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return deviceFailure{stage: failureStageStream}
		}

		candidate := frameCandidate{rgb: frame, observedAt: time.Now().UTC()}
		if err := process(candidate); err != nil {
			return err
		}
	}
}
