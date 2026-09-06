package main

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

type flattenedDeviceScanner struct {
	rtspURI                     sql.NullString
	framePackageIntervalMinutes int
}

func (scanner flattenedDeviceScanner) Scan(destinations ...any) error {
	if len(destinations) != 14 {
		return fmt.Errorf("destination count = %d, want 14", len(destinations))
	}
	*destinations[0].(*int64) = 83
	*destinations[1].(*int64) = 42
	*destinations[2].(*int64) = 17
	*destinations[3].(*bool) = true
	*destinations[4].(*sql.NullString) = scanner.rtspURI
	*destinations[5].(*sql.NullString) = sql.NullString{
		String: "camera user",
		Valid:  true,
	}
	*destinations[6].(*sql.NullString) = sql.NullString{
		String: "camera password",
		Valid:  true,
	}
	*destinations[7].(*int) = 4
	*destinations[8].(*int) = scanner.framePackageIntervalMinutes
	*destinations[9].(*int32) = 1234
	*destinations[10].(*sql.NullInt16) = sql.NullInt16{Int16: 1, Valid: true}
	*destinations[11].(*sql.NullInt16) = sql.NullInt16{Int16: 2, Valid: true}
	*destinations[12].(*sql.NullInt16) = sql.NullInt16{Int16: 3, Valid: true}
	*destinations[13].(*sql.NullInt16) = sql.NullInt16{Int16: 4, Valid: true}
	return nil
}

func TestLoadDevicesStatementUsesFlattenedProductionSchema(t *testing.T) {
	for _, required := range []string{
		"s.organisation_id",
		"d.site_id",
		"d.enabled",
		"d.rtsp_uri",
		"d.rtsp_username",
		"d.rtsp_password",
		"d.capture_fps",
		"d.frame_package_interval_minutes",
		"d.change_threshold_bp",
		"d.line_ax",
		"d.line_ay",
		"d.line_bx",
		"d.line_by",
		"ORDER BY d.id",
	} {
		if !strings.Contains(loadDevicesStatement, required) {
			t.Errorf("device query does not contain %q", required)
		}
	}
	for _, forbidden := range []string{
		"gcs_source_uri",
		"rtsp_config",
		"capture_config",
		"analysis_config",
		"analysis_interval_minutes",
		".status",
	} {
		if strings.Contains(loadDevicesStatement, forbidden) {
			t.Errorf("device query unexpectedly contains %q", forbidden)
		}
	}
}

func TestScanDeviceRecordMapsFlattenedFields(t *testing.T) {
	record, err := scanDeviceRecord(flattenedDeviceScanner{
		rtspURI: sql.NullString{
			String: "rtsp://camera.example/live",
			Valid:  true,
		},
		framePackageIntervalMinutes: 15,
	})
	if err != nil {
		t.Fatalf("scanDeviceRecord() error = %v", err)
	}
	if record.id != 83 || record.organisationID != 42 || record.siteID != 17 {
		t.Fatalf("unexpected identity mapping: %#v", record)
	}
	if !record.enabled || record.rtspURI != "rtsp://camera.example/live" {
		t.Fatalf("unexpected configuration mapping: %#v", record)
	}
	if record.rtspUsername != "camera user" || record.rtspPassword != "camera password" {
		t.Fatalf("unexpected RTSP credential mapping: %#v", record)
	}
	if record.captureFPS != 4 {
		t.Fatalf("captureFPS = %d, want 4", record.captureFPS)
	}
	if record.framePackageIntervalMinutes != 15 {
		t.Fatalf(
			"framePackageIntervalMinutes = %d, want 15",
			record.framePackageIntervalMinutes,
		)
	}
	if record.changeThresholdPercent != 12.34 {
		t.Fatalf(
			"changeThresholdPercent = %v, want 12.34",
			record.changeThresholdPercent,
		)
	}
	if record.lineAX.Int16 != 1 || record.lineAY.Int16 != 2 ||
		record.lineBX.Int16 != 3 || record.lineBY.Int16 != 4 {
		t.Fatalf("unexpected line mapping: %#v", record)
	}
}

func TestDatabaseDeviceRecordMapsNullRTSPFieldsToEmpty(t *testing.T) {
	record := databaseDeviceRecord{
		rtspURI:      sql.NullString{String: "ignored", Valid: false},
		rtspUsername: sql.NullString{String: "ignored", Valid: false},
		rtspPassword: sql.NullString{String: "ignored", Valid: false},
	}.runtimeRecord()
	if record.rtspURI != "" || record.rtspUsername != "" || record.rtspPassword != "" {
		t.Fatalf("NULL RTSP fields were not mapped to empty strings")
	}
}

func TestScanDeviceRecordRejectsNonPositiveFramePackageInterval(t *testing.T) {
	for _, interval := range []int{0, -1} {
		_, err := scanDeviceRecord(flattenedDeviceScanner{
			rtspURI:                     sql.NullString{String: "rtsp://camera.example/live", Valid: true},
			framePackageIntervalMinutes: interval,
		})
		if err == nil {
			t.Errorf("scanDeviceRecord() accepted interval %d", interval)
		}
	}
}

func TestRefreshGatewayControlReportsVersionAndReadsHierarchy(t *testing.T) {
	for _, required := range []string{
		"last_seen_at",
		"reported_version = $2",
		"desired_state",
		"desired_version",
		"restart_requested_at",
		"s.organisation_id",
		"o.enabled",
		"s.enabled",
		"LEFT JOIN public.sites",
		"LEFT JOIN public.organisations",
	} {
		if !strings.Contains(refreshGatewayControlStatement, required) {
			t.Errorf("gateway control query does not contain %q", required)
		}
	}

	setStart := strings.Index(refreshGatewayControlStatement, "SET")
	setEnd := strings.Index(refreshGatewayControlStatement, "WHERE gateway_id")
	if setStart == -1 || setEnd <= setStart {
		t.Fatal("gateway control query has no bounded SET clause")
	}
	setClause := refreshGatewayControlStatement[setStart:setEnd]
	for _, forbidden := range []string{
		"desired_state",
		"desired_version",
		"restart_requested_at",
		"site_id",
		"commission_hash",
	} {
		if strings.Contains(setClause, forbidden) {
			t.Errorf("gateway heartbeat unexpectedly writes %q", forbidden)
		}
	}
}

func TestCommissionHashClearIsConstrained(t *testing.T) {
	for _, required := range []string{
		"UPDATE public.gateways",
		"SET commission_hash = NULL",
		"gateway_id = $1::uuid",
		"commission_hash = $2::bytea",
	} {
		if !strings.Contains(clearCommissionHashStatement, required) {
			t.Errorf("commission clear does not contain %q", required)
		}
	}
	if strings.Contains(clearCommissionHashStatement, "last_seen_at") ||
		strings.Contains(clearCommissionHashStatement, "reported_version") {
		t.Fatal("commission clear modifies unrelated runtime fields")
	}
}

func TestCloudSQLUsesFixedRuntimeDatabaseUser(t *testing.T) {
	if runtimeDatabaseUser != "gateway-runtime@camosbase.iam" {
		t.Fatalf("database user = %q", runtimeDatabaseUser)
	}
}

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
		"desired_state",
		"desired_version",
		"rtsp_uri",
		"enabled",
	} {
		if strings.Contains(statement, forbidden) {
			t.Errorf("runtime fact statement unexpectedly contains %q", forbidden)
		}
	}
}
