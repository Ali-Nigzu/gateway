package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequestCommissionUsesOnlyCommissionIDAndPublicKey(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request %s %s", request.Method, request.Header.Get("Content-Type"))
		}
		defer request.Body.Close()
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		fmt.Fprint(writer, `{"gateway_id":"1d91378f-7b96-4e6f-95b2-1304b728d28f","certificate_pem":"certificate"}`)
	}))
	defer server.Close()

	result, err := requestCommission(
		context.Background(),
		server.Client(),
		server.URL,
		"raw-commission-id",
		"public-key",
	)
	if err != nil || result.GatewayID == "" {
		t.Fatalf("request failed: %#v, %v", result, err)
	}
	if len(received) != 2 || received["commission_id"] != "raw-commission-id" ||
		received["public_key_pem"] != "public-key" {
		t.Fatalf("unexpected commissioning request: %#v", received)
	}
}

func TestCommissionErrorNeverReflectsRawCommissionID(t *testing.T) {
	raw := "do-not-log-this-commission-id"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnprocessableEntity)
		fmt.Fprintf(writer, "invalid %s", raw)
	}))
	defer server.Close()

	_, err := requestCommission(context.Background(), server.Client(), server.URL, raw, "public-key")
	if err == nil {
		t.Fatal("rejection was accepted")
	}
	if strings.Contains(err.Error(), raw) {
		t.Fatalf("raw Commission ID leaked through error: %v", err)
	}
	var classification *commissionHTTPError
	if !errors.As(err, &classification) || !classification.definitiveInvalidID {
		t.Fatalf("rejection was not classified definitively: %v", err)
	}
}

func TestCommissionRetryReusesIdenticalRequestAfterTransientFailure(t *testing.T) {
	var (
		mutex  sync.Mutex
		bodies [][]byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		mutex.Lock()
		bodies = append(bodies, body)
		attempt := len(bodies)
		mutex.Unlock()
		if attempt == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(writer, `{"gateway_id":"1d91378f-7b96-4e6f-95b2-1304b728d28f","certificate_pem":"certificate"}`)
	}))
	defer server.Close()

	_, err := commissionUntilAcceptedAt(
		context.Background(),
		server.Client(),
		server.URL,
		"same-commission-id",
		"same-public-key",
		time.Millisecond,
	)
	if err != nil {
		t.Fatal(err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(bodies) != 2 || string(bodies[0]) != string(bodies[1]) {
		t.Fatalf("retry changed the request: %q", bodies)
	}
}

func TestDefinitiveCommissionRejectionIsConservative(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusGone, http.StatusUnprocessableEntity} {
		if !definitiveCommissionRejection(status) {
			t.Fatalf("status %d should reject the ID", status)
		}
	}
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusRequestTimeout, http.StatusConflict, http.StatusTooManyRequests, http.StatusInternalServerError} {
		if definitiveCommissionRejection(status) {
			t.Fatalf("transient/auth status %d destroyed resumable state", status)
		}
	}
}

func TestCommissioningGuardRejectsEveryRemovalMarkerState(t *testing.T) {
	for _, test := range []struct {
		name            string
		marker          []byte
		markerDirectory bool
	}{
		{name: "valid v1", marker: mustRemovalMarker(t, testRemovalGatewayID)},
		{name: "legacy", marker: []byte(legacyRemovalMarkerContents)},
		{name: "different GatewayID", marker: mustRemovalMarker(t, testOtherRemovalGatewayID)},
		{name: "malformed", marker: []byte("malformed\n")},
		{name: "unreadable", markerDirectory: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
			if test.markerDirectory {
				if err := os.Mkdir(paths.removalPending, 0o700); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(paths.removalPending, test.marker, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := ensureCommissioningAllowed(paths); err == nil {
				t.Fatal("commissioning was allowed with terminal removal state")
			}
		})
	}
}

func TestCommissioningGuardAllowsAbsentRemovalMarker(t *testing.T) {
	directory := t.TempDir()
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	if err := ensureCommissioningAllowed(paths); err != nil {
		t.Fatal(err)
	}
}

func TestCommissionCompletionRefusesRemovalBeforeReportingSuccess(t *testing.T) {
	directory := t.TempDir()
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	if err := os.WriteFile(
		paths.removalPending,
		mustRemovalMarker(t, testRemovalGatewayID),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := waitForCommissionCompletion(
		context.Background(),
		paths,
		hashCommissionID("commission-id"),
		time.Millisecond,
	); err == nil {
		t.Fatal("completion succeeded while terminal removal was pending")
	}
}

func TestCommissionCompletionStopsWhenRemovalAppears(t *testing.T) {
	directory := t.TempDir()
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	expected := hashCommissionID("commission-id")
	if err := os.WriteFile(paths.commissionHash, expected.encoded(), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- waitForCommissionCompletion(ctx, paths, expected, time.Millisecond)
	}()
	if err := os.WriteFile(
		paths.removalPending,
		[]byte(legacyRemovalMarkerContents),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err == nil {
		t.Fatal("completion did not stop when terminal removal appeared")
	}
}

func TestPreparedCommissionRejectsRecreatedIdentityGeneration(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "identity")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	privateKey, err := generateGatewayPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	privateKeyPEM, err := marshalGatewayPrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	hash := hashCommissionID("same-commission-id")
	if err := os.WriteFile(paths.privateKey, privateKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.commissionHash, hash.encoded(), 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := captureIdentityDirectoryGeneration(paths)
	if err != nil {
		t.Fatal(err)
	}
	prepared := preparedCommission{
		paths:               paths,
		hash:                hash,
		privateKey:          privateKey,
		directoryGeneration: generation,
	}
	if err := validatePreparedCommissionFiles(prepared); err != nil {
		t.Fatalf("current prepared commission was rejected: %v", err)
	}

	retired := directory + ".retired"
	if err := os.Rename(directory, retired); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	// Even identical logical contents cannot make the replacement directory
	// the generation whose public key was sent to the Commission Service.
	if err := os.WriteFile(paths.privateKey, privateKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.commissionHash, hash.encoded(), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validatePreparedCommissionFiles(prepared); err == nil {
		t.Fatal("recreated identity generation accepted an old commissioning response")
	}
}
