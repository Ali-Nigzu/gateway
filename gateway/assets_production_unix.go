//go:build production && ((darwin && (amd64 || arm64)) || (linux && amd64))

package main

import _ "embed"

//go:embed ffmpeg
var embeddedFFmpeg []byte
