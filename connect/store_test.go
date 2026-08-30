package connect

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestServiceAccountEmailDerivesIAMDatabaseUsernameSource(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(filename, []byte(`{
  "client_email": "gateway-service@camosbase.iam.gserviceaccount.com",
  "private_key": "FAKE-PRIVATE-KEY-MUST-NOT-LEAK"
}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	email, err := serviceAccountEmail(filename)
	if err != nil {
		t.Fatalf("serviceAccountEmail() error = %v", err)
	}
	if email != "gateway-service@camosbase.iam.gserviceaccount.com" {
		t.Fatalf("email = %q", email)
	}
	if username := strings.TrimSuffix(email, ".gserviceaccount.com"); username != "gateway-service@camosbase.iam" {
		t.Fatalf("derived IAM database username = %q", username)
	}
}

func TestServiceAccountErrorsAreSanitized(t *testing.T) {
	secret := "FAKE-PRIVATE-KEY-MUST-NOT-LEAK"
	filename := filepath.Join(t.TempDir(), "sa.json")
	if err := os.WriteFile(filename, []byte(`{"private_key":"`+secret+`"}`), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_, err := serviceAccountEmail(filename)
	if err == nil || err.Error() != "sa.json is invalid" || strings.Contains(err.Error(), secret) {
		t.Fatalf("serviceAccountEmail() error = %v", err)
	}
}

func TestPostgresStoreLoadsSiteAndDevicesSeparately(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer database.Close()
	store := &postgresStore{database: database}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(siteSelectSQL())).WithArgs(int64(71)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status"}).AddRow(71, "Test Site", "enabled"))
	mock.ExpectQuery(regexp.QuoteMeta(deviceSelectSQL())).WithArgs(int64(71)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "site_id", "status", "gcs_source_uri", "rtsp_config", "capture_config"}).
			AddRow(37, "North", 71, "enabled", "gs://bucket/north", []byte(`{"uri":"rtsp://north/live"}`), []byte(`{"fps":3,"change_threshold_percent":0.5}`)).
			AddRow(811, "South", 71, "disabled", "gs://bucket/south", []byte(`{"uri":"rtsp://south/live"}`), []byte(`{"fps":3,"change_threshold_percent":0.5}`)).
			AddRow(912, "Unprovisioned", 71, "enabled", nil, []byte(`null`), []byte(`{"fps":3,"change_threshold_percent":0.5}`)))
	mock.ExpectCommit()

	site, devices, err := store.LoadSite(context.Background(), 71)
	if err != nil {
		t.Fatalf("LoadSite() error = %v", err)
	}
	if site.ID != 71 || site.Name != "Test Site" || len(devices) != 3 {
		t.Fatalf("LoadSite() = %#v, %#v", site, devices)
	}
	if devices[0].ID != 37 || devices[1].ID != 811 {
		t.Fatalf("actual Device IDs not preserved: %#v", devices)
	}
	if devices[2].GCSURI != "" || !isNullJSON(devices[2].RTSPConfig) {
		t.Fatalf("NULL Device provisioning did not remain locally invalid: %#v", devices[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestPostgresStoreDistinguishesMissingSiteFromZeroDevices(t *testing.T) {
	t.Run("missing Site", func(t *testing.T) {
		database, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New(): %v", err)
		}
		defer database.Close()
		store := &postgresStore{database: database}
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta(siteSelectSQL())).WithArgs(int64(1)).
			WillReturnError(sql.ErrNoRows)
		mock.ExpectRollback()
		_, _, err = store.LoadSite(context.Background(), 1)
		if !errors.Is(err, errSiteNotFound) {
			t.Fatalf("LoadSite() error = %v, want errSiteNotFound", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("SQL expectations: %v", err)
		}
	})

	t.Run("zero Devices", func(t *testing.T) {
		database, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock.New(): %v", err)
		}
		defer database.Close()
		store := &postgresStore{database: database}
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta(siteSelectSQL())).WithArgs(int64(1)).
			WillReturnRows(sqlmock.NewRows([]string{"id", "name", "status"}).AddRow(1, "Empty", "enabled"))
		mock.ExpectQuery(regexp.QuoteMeta(deviceSelectSQL())).WithArgs(int64(1)).
			WillReturnRows(sqlmock.NewRows([]string{"id", "name", "site_id", "status", "gcs_source_uri", "rtsp_config", "capture_config"}))
		mock.ExpectCommit()
		_, devices, err := store.LoadSite(context.Background(), 1)
		if err != nil || len(devices) != 0 {
			t.Fatalf("LoadSite() devices = %#v, error = %v", devices, err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("SQL expectations: %v", err)
		}
	})
}

func TestGatewayLogsAreAppendOnlyAndPreserveDeviceNullability(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer database.Close()
	store := &postgresStore{database: database}
	at := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	insert := regexp.QuoteMeta(eventInsertSQL())

	mock.ExpectExec(insert).
		WithArgs(int64(1), nil, at, eventGatewaySiteLoaded, "Site configuration loaded", `{}`).
		WillReturnResult(sqlmock.NewResult(1, 1))
	deviceID := int64(811)
	for count := 0; count < 2; count++ {
		mock.ExpectExec(insert).
			WithArgs(int64(1), int64(811), at, eventDeviceRTSPFailed, "RTSP stream failed", `{"reason_code":"connection_failed"}`).
			WillReturnResult(sqlmock.NewResult(int64(2+count), 1))
	}

	if err := store.RecordEvent(context.Background(), gatewayEvent{
		SiteID: 1, OccurredAt: at, Type: eventGatewaySiteLoaded, Message: "Site configuration loaded",
	}); err != nil {
		t.Fatalf("gateway RecordEvent() error = %v", err)
	}
	for count := 0; count < 2; count++ {
		if err := store.RecordEvent(context.Background(), gatewayEvent{
			SiteID: 1, DeviceID: &deviceID, OccurredAt: at,
			Type: eventDeviceRTSPFailed, Message: "RTSP stream failed",
			Details: map[string]any{"reason_code": "connection_failed"},
		}); err != nil {
			t.Fatalf("device RecordEvent(%d) error = %v", count, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestGatewayLogRejectsRawOrUnknownDetailsBeforeSQL(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer database.Close()
	store := &postgresStore{database: database}
	for _, event := range []gatewayEvent{
		{SiteID: 1, Type: "device.unknown", Details: map[string]any{}},
		{SiteID: 1, Type: eventDeviceRTSPFailed, Details: map[string]any{"raw_error": "rtsp://user:secret@camera/live"}},
	} {
		if err := store.RecordEvent(context.Background(), event); err == nil || err.Error() != "gateway log event invalid" {
			t.Fatalf("RecordEvent(%#v) error = %v", event, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unexpected SQL: %v", err)
	}
}

func TestRuntimeFactUpdatesUseMonotonicDeviceScopedSQL(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer database.Close()
	store := &postgresStore{database: database}
	newer := time.Date(2026, 8, 30, 12, 2, 0, 0, time.UTC)
	older := newer.Add(-time.Minute)

	connectedSQL := regexp.QuoteMeta(markConnectedSQL())
	seenSQL := regexp.QuoteMeta(advanceFrameSeenSQL())
	for _, at := range []time.Time{newer, older} {
		mock.ExpectExec(connectedSQL).WithArgs(int64(37), int64(1), at).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := store.MarkConnected(context.Background(), 1, 37, at); err != nil {
			t.Fatalf("MarkConnected(%v) error = %v", at, err)
		}
		mock.ExpectExec(seenSQL).WithArgs(int64(37), int64(1), at).
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := store.AdvanceFrameSeen(context.Background(), 1, 37, at); err != nil {
			t.Fatalf("AdvanceFrameSeen(%v) error = %v", at, err)
		}
	}
	if !strings.Contains(markConnectedSQL(), "GREATEST") || !strings.Contains(advanceFrameSeenSQL(), "GREATEST") {
		t.Fatal("runtime fact SQL is not monotonic")
	}
	if !strings.Contains(markConnectedSQL(), "site_id = $2") || !strings.Contains(advanceFrameSeenSQL(), "site_id = $2") {
		t.Fatal("runtime fact SQL does not validate Device Site ownership")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestSuccessfulUploadAtomicallyAdvancesDeviceAndSiteFacts(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer database.Close()
	store := &postgresStore{database: database}
	captured := time.Date(2026, 8, 30, 12, 1, 0, 0, time.UTC)
	completed := captured.Add(250 * time.Millisecond)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(markUploadedDeviceSQL())).
		WithArgs(int64(811), int64(1), captured, completed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(markUploadedSiteSQL())).
		WithArgs(int64(1), completed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.MarkUploaded(context.Background(), 1, 811, captured, completed); err != nil {
		t.Fatalf("MarkUploaded() error = %v", err)
	}
	if !strings.Contains(markUploadedDeviceSQL(), "GREATEST") || !strings.Contains(markUploadedSiteSQL(), "GREATEST") {
		t.Fatal("upload facts are not monotonic")
	}
	if strings.Contains(strings.ToLower(markUploadedSiteSQL()), "now()") {
		t.Fatal("Site last-seen SQL uses a heartbeat/database clock")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func TestUploadFactTransactionRollsBackAsAUnit(t *testing.T) {
	database, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New(): %v", err)
	}
	defer database.Close()
	store := &postgresStore{database: database}
	captured := time.Now().UTC()
	completed := captured.Add(time.Second)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(markUploadedDeviceSQL())).
		WithArgs(int64(37), int64(1), captured, completed).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(markUploadedSiteSQL())).
		WithArgs(int64(1), completed).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	err = store.MarkUploaded(context.Background(), 1, 37, captured, completed)
	if err == nil || err.Error() != "database upload fact transaction failed" {
		t.Fatalf("MarkUploaded() error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("SQL expectations: %v", err)
	}
}

func siteSelectSQL() string {
	return `SELECT id, name, status
FROM public.sites
WHERE id = $1`
}

func deviceSelectSQL() string {
	return `SELECT id, name, site_id, status, gcs_source_uri, rtsp_config, capture_config
FROM public.devices
WHERE site_id = $1
ORDER BY id`
}

func eventInsertSQL() string {
	return `INSERT INTO public.gateway_logs (
    site_id,
    device_id,
    occurred_at,
    event_type,
    message,
    details
)
VALUES ($1, $2, $3, $4, $5, $6::jsonb)`
}

func markConnectedSQL() string {
	return `UPDATE public.devices
SET
    last_connected_at = GREATEST(COALESCE(last_connected_at, $3), $3),
    last_frame_seen_at = GREATEST(COALESCE(last_frame_seen_at, $3), $3)
WHERE id = $1
  AND site_id = $2`
}

func advanceFrameSeenSQL() string {
	return `UPDATE public.devices
SET last_frame_seen_at = GREATEST(COALESCE(last_frame_seen_at, $3), $3)
WHERE id = $1
  AND site_id = $2`
}

func markUploadedDeviceSQL() string {
	return `UPDATE public.devices
SET
    last_frame_seen_at = GREATEST(COALESCE(last_frame_seen_at, $3), $3),
    last_frame_uploaded_at = GREATEST(COALESCE(last_frame_uploaded_at, $4), $4)
WHERE id = $1
  AND site_id = $2`
}

func markUploadedSiteSQL() string {
	return `UPDATE public.sites
SET gateway_last_seen_at = GREATEST(COALESCE(gateway_last_seen_at, $2), $2)
WHERE id = $1`
}
