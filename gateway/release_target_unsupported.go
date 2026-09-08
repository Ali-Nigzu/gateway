//go:build (!windows && !darwin && !linux) || (windows && !amd64) || (linux && !amd64) || (darwin && !amd64 && !arm64)

package main

const currentGatewayReleaseTarget = "unsupported"
