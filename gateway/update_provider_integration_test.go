package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestRealArtifactRegistryReleaseContract is deliberately opt-in. It verifies
// the same Generic Artifact Registry resource identity and download route used
// by the appliance against all four approved local release binaries.
//
// Required environment:
//
//	CAMOS_AR_INTEGRATION=1
//	CAMOS_AR_ACCESS_TOKEN=<short-lived read token>
//	CAMOS_GATEWAY_RELEASE_DIR=<directory containing all four artifacts>
//
// Optional:
//
//	CAMOS_GATEWAY_RELEASE_VERSION=<defaults to BuildVersion>
func TestRealArtifactRegistryReleaseContract(t *testing.T) {
	if os.Getenv("CAMOS_AR_INTEGRATION") != "1" {
		t.Skip("set CAMOS_AR_INTEGRATION=1 to verify the real provider contract")
	}
	token := os.Getenv("CAMOS_AR_ACCESS_TOKEN")
	releaseDirectory := os.Getenv("CAMOS_GATEWAY_RELEASE_DIR")
	if token == "" || releaseDirectory == "" {
		t.Fatal("CAMOS_AR_ACCESS_TOKEN and CAMOS_GATEWAY_RELEASE_DIR are required")
	}
	version := os.Getenv("CAMOS_GATEWAY_RELEASE_VERSION")
	if version == "" {
		version = BuildVersion
	}
	if err := validateArtifactVersion(version); err != nil {
		t.Fatal(err)
	}
	if version != BuildVersion {
		t.Fatalf("provider version %q does not match verifier BuildVersion %q", version, BuildVersion)
	}
	localIdentities := verifyLocalReleaseSet(t, releaseDirectory, version)

	client := &http.Client{Transport: bearerRoundTripper{token: token, base: http.DefaultTransport}}
	ctx := context.Background()
	files, err := listArtifactVersion(ctx, client, version)
	if err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{"windows-amd64.exe", "darwin-arm64", "darwin-amd64", "linux-amd64"} {
		t.Run(target, func(t *testing.T) {
			localIdentity := localIdentities[target]
			file, providerHash, err := selectArtifactFile(files, version, target)
			if err != nil {
				t.Fatal(err)
			}
			providerSize, err := artifactExpectedSize(file.SizeBytes)
			if err != nil {
				t.Fatal(err)
			}
			if providerSize != localIdentity.Size || !bytes.Equal(providerHash, localIdentity.SHA256[:]) {
				t.Fatal("provider metadata does not match the approved local release")
			}

			downloadedPath := filepath.Join(t.TempDir(), target)
			if err := downloadGatewayCandidateForTarget(
				ctx,
				client,
				version,
				downloadedPath,
				target,
				gatewayArtifactTransferPolicy,
				inspectGatewayCandidate,
			); err != nil {
				t.Fatalf("runtime provider download path failed: %v", err)
			}
			downloadedIdentity, err := inspectGatewayCandidate(downloadedPath, version, target)
			if err != nil {
				t.Fatalf("downloaded release identity: %v", err)
			}
			if downloadedIdentity.SHA256 != localIdentity.SHA256 {
				t.Fatal("downloaded release does not match the approved local executable")
			}
		})
	}
}

type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (transport bearerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	if transport.token == "" {
		return nil, fmt.Errorf("Artifact Registry access token is missing")
	}
	clone := request.Clone(request.Context())
	clone.Header = request.Header.Clone()
	clone.Header.Set("Authorization", "Bearer "+transport.token)
	return transport.base.RoundTrip(clone)
}
