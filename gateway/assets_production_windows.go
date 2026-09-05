//go:build production && windows && amd64

package main

import _ "embed"

//go:embed ffmpeg.exe
var embeddedFFmpeg []byte
