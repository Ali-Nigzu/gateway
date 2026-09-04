//go:build production && windows

package main

import _ "embed"

//go:embed ffmpeg.exe
var embeddedFFmpeg []byte
