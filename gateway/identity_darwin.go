//go:build darwin

package main

const (
	gatewayIdentityDirectory = "/Library/Application Support/camOS Gateway"
	gatewayIdentityPath      = gatewayIdentityDirectory + "/GatewayID"
)
