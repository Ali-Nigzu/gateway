//go:build !windows

package main

import "context"

func runGatewayApplication(
	ctx context.Context,
	devices []deviceRecord,
	credentials *runtimeCredentials,
) error {
	return startGateway(ctx, devices, credentials)
}
