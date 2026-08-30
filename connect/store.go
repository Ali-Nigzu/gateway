package connect

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

const (
	cloudSQLInstance = "camosbase:europe-west2:camos-prod-postgres"
	cloudSQLDatabase = "camos_prod"

	poolMaxOpenConnections = 8
	poolMaxIdleConnections = 4
	poolMaxIdleTime        = 5 * time.Minute
	poolMaxLifetime        = 55 * time.Minute
)

const (
	eventGatewayCommissioningStarted = "gateway.commissioning_started"
	eventGatewayDatabaseConnected    = "gateway.database_connected"
	eventGatewaySiteLoaded           = "gateway.site_loaded"
	eventGatewayCommissioningFailed  = "gateway.commissioning_failed"
	eventGatewayShutdownRequested    = "gateway.shutdown_requested"
	eventGatewayShutdownCompleted    = "gateway.shutdown_completed"
	eventGatewayShutdownFailed       = "gateway.shutdown_failed"

	eventDeviceConfigurationInvalid  = "device.configuration_invalid"
	eventDeviceWorkerSkipped         = "device.worker_skipped"
	eventDeviceWorkerStarted         = "device.worker_started"
	eventDeviceRTSPConnectionAttempt = "device.rtsp_connection_attempt"
	eventDeviceRTSPConnected         = "device.rtsp_connected"
	eventDeviceRTSPFailed            = "device.rtsp_failed"
	eventDeviceFFmpegStarted         = "device.ffmpeg_started"
	eventDeviceFFmpegExited          = "device.ffmpeg_exited"
	eventDeviceFramePipelineFailed   = "device.frame_pipeline_failed"
	eventDeviceGCSUploadFailed       = "device.gcs_upload_failed"
	eventDeviceDatabaseWriteFailed   = "device.database_write_failed"
	eventDeviceWorkerStopped         = "device.worker_stopped"
)

var errSiteNotFound = errors.New("site not found")

type siteRecord struct {
	ID     int64
	Name   string
	Status string
}

type deviceRecord struct {
	ID            int64
	Name          string
	SiteID        int64
	Status        string
	GCSURI        string
	RTSPConfig    []byte
	CaptureConfig []byte
}

type gatewayEvent struct {
	SiteID     int64
	DeviceID   *int64
	OccurredAt time.Time
	Type       string
	Message    string
	Details    map[string]any
}

type gatewayStore interface {
	LoadSite(context.Context, int64) (siteRecord, []deviceRecord, error)
	RecordEvent(context.Context, gatewayEvent) error
	MarkConnected(context.Context, int64, int64, time.Time) error
	AdvanceFrameSeen(context.Context, int64, int64, time.Time) error
	MarkUploaded(context.Context, int64, int64, time.Time, time.Time) error
	Close() error
}

type postgresStore struct {
	database *sql.DB
	dialer   *cloudsqlconn.Dialer
}

func newPostgresStore(ctx context.Context, credentialPath string) (gatewayStore, error) {
	email, err := serviceAccountEmail(credentialPath)
	if err != nil {
		return nil, err
	}
	databaseUser := strings.TrimSuffix(email, ".gserviceaccount.com")

	dialer, err := cloudsqlconn.NewDialer(
		ctx,
		cloudsqlconn.WithCredentialsFile(credentialPath),
		cloudsqlconn.WithIAMAuthN(),
	)
	if err != nil {
		return nil, errors.New("Cloud SQL authentication failed")
	}

	config, err := pgx.ParseConfig("sslmode=disable")
	if err != nil {
		_ = dialer.Close()
		return nil, errors.New("Cloud SQL configuration failed")
	}
	config.User = databaseUser
	config.Database = cloudSQLDatabase
	config.DialFunc = func(ctx context.Context, _ string, _ string) (net.Conn, error) {
		return dialer.Dial(ctx, cloudSQLInstance)
	}

	database := stdlib.OpenDB(*config)
	database.SetMaxOpenConns(poolMaxOpenConnections)
	database.SetMaxIdleConns(poolMaxIdleConnections)
	database.SetConnMaxIdleTime(poolMaxIdleTime)
	database.SetConnMaxLifetime(poolMaxLifetime)

	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		_ = dialer.Close()
		return nil, errors.New("Cloud SQL connection failed")
	}

	return &postgresStore{database: database, dialer: dialer}, nil
}

func serviceAccountEmail(filename string) (string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return "", errors.New("sa.json is not accessible")
	}
	var metadata struct {
		ClientEmail string `json:"client_email"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", errors.New("sa.json is invalid")
	}
	email := strings.TrimSpace(metadata.ClientEmail)
	if email == "" || email != strings.ToLower(email) || !strings.HasSuffix(email, ".gserviceaccount.com") {
		return "", errors.New("sa.json is invalid")
	}
	return email, nil
}

func (store *postgresStore) LoadSite(ctx context.Context, siteID int64) (siteRecord, []deviceRecord, error) {
	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return siteRecord{}, nil, errors.New("database configuration read failed")
	}
	defer transaction.Rollback()

	var site siteRecord
	err = transaction.QueryRowContext(
		ctx,
		`SELECT id, name, status
FROM public.sites
WHERE id = $1`,
		siteID,
	).Scan(&site.ID, &site.Name, &site.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return siteRecord{}, nil, errSiteNotFound
	}
	if err != nil {
		return siteRecord{}, nil, errors.New("database site configuration query failed")
	}

	rows, err := transaction.QueryContext(
		ctx,
		`SELECT id, name, site_id, status, gcs_source_uri, rtsp_config, capture_config
FROM public.devices
WHERE site_id = $1
ORDER BY id`,
		siteID,
	)
	if err != nil {
		return siteRecord{}, nil, errors.New("database Device configuration query failed")
	}
	defer rows.Close()

	devices := make([]deviceRecord, 0)
	for rows.Next() {
		var device deviceRecord
		var gcsURI sql.NullString
		if err := rows.Scan(
			&device.ID,
			&device.Name,
			&device.SiteID,
			&device.Status,
			&gcsURI,
			&device.RTSPConfig,
			&device.CaptureConfig,
		); err != nil {
			return siteRecord{}, nil, errors.New("database Device configuration query failed")
		}
		if gcsURI.Valid {
			device.GCSURI = gcsURI.String
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		return siteRecord{}, nil, errors.New("database Device configuration query failed")
	}
	if err := rows.Close(); err != nil {
		return siteRecord{}, nil, errors.New("database Device configuration query failed")
	}
	if err := transaction.Commit(); err != nil {
		return siteRecord{}, nil, errors.New("database configuration read failed")
	}

	return site, devices, nil
}

func (store *postgresStore) RecordEvent(ctx context.Context, event gatewayEvent) error {
	if !knownEventType(event.Type) || !safeEventDetails(event.Details) {
		return errors.New("gateway log event invalid")
	}
	details := event.Details
	if details == nil {
		details = map[string]any{}
	}
	encoded, err := json.Marshal(details)
	if err != nil {
		return errors.New("gateway log event invalid")
	}

	var deviceID any
	if event.DeviceID != nil {
		deviceID = *event.DeviceID
	}
	_, err = store.database.ExecContext(
		ctx,
		`INSERT INTO public.gateway_logs (
    site_id,
    device_id,
    occurred_at,
    event_type,
    message,
    details
)
VALUES ($1, $2, $3, $4, $5, $6::jsonb)`,
		event.SiteID,
		deviceID,
		event.OccurredAt.UTC(),
		event.Type,
		event.Message,
		string(encoded),
	)
	if err != nil {
		return errors.New("gateway log write failed")
	}
	return nil
}

func (store *postgresStore) MarkConnected(ctx context.Context, siteID, deviceID int64, observedAt time.Time) error {
	result, err := store.database.ExecContext(
		ctx,
		`UPDATE public.devices
SET
    last_connected_at = GREATEST(COALESCE(last_connected_at, $3), $3),
    last_frame_seen_at = GREATEST(COALESCE(last_frame_seen_at, $3), $3)
WHERE id = $1
  AND site_id = $2`,
		deviceID,
		siteID,
		observedAt.UTC(),
	)
	if err != nil || !exactlyOneRow(result) {
		return errors.New("database runtime fact update failed")
	}
	return nil
}

func (store *postgresStore) AdvanceFrameSeen(ctx context.Context, siteID, deviceID int64, observedAt time.Time) error {
	result, err := store.database.ExecContext(
		ctx,
		`UPDATE public.devices
SET last_frame_seen_at = GREATEST(COALESCE(last_frame_seen_at, $3), $3)
WHERE id = $1
  AND site_id = $2`,
		deviceID,
		siteID,
		observedAt.UTC(),
	)
	if err != nil || !exactlyOneRow(result) {
		return errors.New("database runtime fact update failed")
	}
	return nil
}

func (store *postgresStore) MarkUploaded(
	ctx context.Context,
	siteID, deviceID int64,
	capturedAt, completedAt time.Time,
) error {
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return errors.New("database upload fact transaction failed")
	}
	defer transaction.Rollback()

	deviceResult, err := transaction.ExecContext(
		ctx,
		`UPDATE public.devices
SET
    last_frame_seen_at = GREATEST(COALESCE(last_frame_seen_at, $3), $3),
    last_frame_uploaded_at = GREATEST(COALESCE(last_frame_uploaded_at, $4), $4)
WHERE id = $1
  AND site_id = $2`,
		deviceID,
		siteID,
		capturedAt.UTC(),
		completedAt.UTC(),
	)
	if err != nil || !exactlyOneRow(deviceResult) {
		return errors.New("database upload fact transaction failed")
	}

	siteResult, err := transaction.ExecContext(
		ctx,
		// gateway_last_seen_at is deliberately upload-driven, not a process
		// heartbeat. The completion timestamp is shared with the Device fact.
		`UPDATE public.sites
SET gateway_last_seen_at = GREATEST(COALESCE(gateway_last_seen_at, $2), $2)
WHERE id = $1`,
		siteID,
		completedAt.UTC(),
	)
	if err != nil || !exactlyOneRow(siteResult) {
		return errors.New("database upload fact transaction failed")
	}
	if err := transaction.Commit(); err != nil {
		return errors.New("database upload fact transaction failed")
	}
	return nil
}

func exactlyOneRow(result sql.Result) bool {
	if result == nil {
		return false
	}
	rows, err := result.RowsAffected()
	return err == nil && rows == 1
}

func (store *postgresStore) Close() error {
	databaseErr := store.database.Close()
	dialerErr := store.dialer.Close()
	return errors.Join(databaseErr, dialerErr)
}

func knownEventType(eventType string) bool {
	switch eventType {
	case eventGatewayCommissioningStarted,
		eventGatewayDatabaseConnected,
		eventGatewaySiteLoaded,
		eventGatewayCommissioningFailed,
		eventGatewayShutdownRequested,
		eventGatewayShutdownCompleted,
		eventGatewayShutdownFailed,
		eventDeviceConfigurationInvalid,
		eventDeviceWorkerSkipped,
		eventDeviceWorkerStarted,
		eventDeviceRTSPConnectionAttempt,
		eventDeviceRTSPConnected,
		eventDeviceRTSPFailed,
		eventDeviceFFmpegStarted,
		eventDeviceFFmpegExited,
		eventDeviceFramePipelineFailed,
		eventDeviceGCSUploadFailed,
		eventDeviceDatabaseWriteFailed,
		eventDeviceWorkerStopped:
		return true
	default:
		return false
	}
}

func safeEventDetails(details map[string]any) bool {
	for key, value := range details {
		switch key {
		case "reason_code", "site_status", "device_status":
			if _, ok := value.(string); !ok {
				return false
			}
		case "ffmpeg_exit_code", "received_bytes", "worker_count", "valid_devices", "invalid_devices", "disabled_devices":
			switch value.(type) {
			case int, int32, int64:
			default:
				return false
			}
		default:
			return false
		}
	}
	return true
}
