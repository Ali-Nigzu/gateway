package main

import (
	"strings"
	"testing"
)

func TestRuntimeFactStatementZeroDevices(t *testing.T) {
	if statement := runtimeFactStatement(0); statement != "" {
		t.Fatalf("runtimeFactStatement(0) = %q, want empty", statement)
	}
}

func TestRuntimeFactStatementContainsOnlyDeviceFacts(t *testing.T) {
	statement := runtimeFactStatement(2)
	for _, required := range []string{
		"UPDATE public.devices",
		"$1::bigint",
		"$4::timestamptz",
		"$5::bigint",
		"$8::timestamptz",
		"last_connected_at",
		"last_frame_seen_at",
		"last_frame_uploaded_at",
	} {
		if !strings.Contains(statement, required) {
			t.Errorf("runtime fact statement does not contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"public.sites",
		"gateway_last_seen_at",
		"d.site_id",
	} {
		if strings.Contains(statement, forbidden) {
			t.Errorf("runtime fact statement unexpectedly contains %q", forbidden)
		}
	}
}
