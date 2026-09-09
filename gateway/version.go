package main

import "runtime"

// BuildVersion is the immutable camOS Gateway release represented by this
// source tree. Artifact Registry generic package versions use the same value.
const BuildVersion = "1.0"

const (
	gatewayGoModule             = "camos-gateway"
	gatewayReleaseStampPrefix   = "CAMOS_GATEWAY_RELEASE_STAMP_V1|"
	gatewayReleaseStampSuffix   = "|END_CAMOS_GATEWAY_RELEASE_STAMP"
	embeddedGatewayReleaseStamp = gatewayReleaseStampPrefix +
		"module=" + gatewayGoModule +
		"|version=" + BuildVersion +
		"|target=" + currentGatewayReleaseTarget +
		gatewayReleaseStampSuffix
)

// Keep the identity as data in the executable. Candidate validation reads this
// fixed-format stamp without starting downloaded code.
var embeddedGatewayReleaseStampData = embeddedGatewayReleaseStamp

//go:noinline
func retainGatewayReleaseStamp() {
	// Keep a concrete, contiguous stamp in every platform linker output. A
	// no-inline entry-point reference prevents Darwin dead-data elimination.
	// Keep the string value, not merely the address of its descriptor: the
	// Darwin linker can otherwise prove that the backing bytes are never read
	// and discard them while retaining the zero-cost address expression.
	runtime.KeepAlive(embeddedGatewayReleaseStampData)
}

func expectedGatewayReleaseStamp(version, target string) string {
	return gatewayReleaseStampPrefix +
		"module=" + gatewayGoModule +
		"|version=" + version +
		"|target=" + target +
		gatewayReleaseStampSuffix
}

func currentGatewayReleaseStamp() string {
	return embeddedGatewayReleaseStampData
}
