//go:build production && (darwin || linux)

package main

import _ "embed"

//go:embed ffmpeg
var embeddedFFmpeg []byte
