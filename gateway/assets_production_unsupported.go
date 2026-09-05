//go:build production && !((windows && amd64) || (darwin && (amd64 || arm64)) || (linux && amd64))

package main

// Keep unsupported production tuples as compile-time failures even when
// someone bypasses the release scripts and invokes go build directly.
var embeddedFFmpeg []byte
var _ = unsupportedGatewayProductionTarget
