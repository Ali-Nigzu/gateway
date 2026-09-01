package main

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
)

func fillComparison(rgb []byte, comparison *[comparisonCellCount]byte) {
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
}

func materiallyChanged(
	baseline, candidate *[comparisonCellCount]byte,
	thresholdPercent float64,
) bool {
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
		"-loglevel", "error",
		"-nostdin",
		"-rtsp_transport", "tcp",
		"-i", sourceURI,
		"-map", "0:v:0",
		"-vf", filter,
		"-f", "rawvideo",
		"-pix_fmt", "rgb24",
		"pipe:1",
	}
}

func streamRTSP(
	ctx context.Context,
	sourceURI string,
	fps int,
	frame []byte,
	process func([]byte, time.Time) error,
) error {
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(attemptCtx, "ffmpeg", ffmpegArguments(sourceURI, fps)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		cancel()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	silence := time.AfterFunc(frameSilenceTimeout, cancel)
	defer silence.Stop()
	for {
		if _, err := io.ReadFull(stdout, frame); err != nil {
			return err
		}
		if err := attemptCtx.Err(); err != nil {
			return err
		}
		if !silence.Stop() {
			return context.DeadlineExceeded
		}
		silence.Reset(frameSilenceTimeout)
		if err := process(frame, time.Now().UTC()); err != nil {
			return err
		}
	}
}
