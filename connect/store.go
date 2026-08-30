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
	eventGatewayStarted = "gateway.started"
	eventDeviceStarted  = "device.started"
	eventDeviceFailed   = "device.failed"
	eventDeviceStopped  = "device.stopped"
	eventGatewayStopped = "gateway.stopped"

	failureStageStream   = "stream"
	failureStageUpload   = "upload"
	failureStageDatabase = "database"
	failureStageCrash    = "crash"
)

type siteRecord struct {
	id     int64
	status string
}

type deviceRecord struct {
	id            int64
	status        string
	gcsURI        string
	rtspConfig    []byte
	captureConfig []byte
}

type gatewayEvent struct {
	siteID     int64
	deviceID   *int64
	occurredAt time.Time
	eventType  string
	message    string
	stage      string
}

type postgresStore struct {
	database *sql.DB
	dialer   *cloudsqlconn.Dialer
}

func newPostgresStore(ctx context.Context, credentialPath string) (*postgresStore, error) {
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
		return nil, errors.New("database startup failed")
	}

	config, err := pgx.ParseConfig("sslmode=disable")
	if err != nil {
		_ = dialer.Close()
		return nil, errors.New("database startup failed")
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
		return nil, errors.New("database startup failed")
	}

	return &postgresStore{database: database, dialer: dialer}, nil
}

func serviceAccountEmail(filename string) (string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return "", errors.New("sa.json unavailable")
	}
	var metadata struct {
		ClientEmail string `json:"client_email"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil {
		return "", errors.New("sa.json unavailable")
	}
	return strings.TrimSpace(metadata.ClientEmail), nil
}

func (store *postgresStore) LoadSite(ctx context.Context, siteID int64) (siteRecord, []deviceRecord, error) {
	transaction, err := store.database.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelRepeatableRead,
		ReadOnly:  true,
	})
	if err != nil {
		return siteRecord{}, nil, errors.New("site load failed")
	}
	defer transaction.Rollback()

	var site siteRecord
	if err := transaction.QueryRowContext(
		ctx,
		`SELECT id, status
FROM public.sites
WHERE id = $1`,
		siteID,
	).Scan(&site.id, &site.status); err != nil {
		return siteRecord{}, nil, errors.New("site load failed")
	}

	rows, err := transaction.QueryContext(
		ctx,
		`SELECT id, status, gcs_source_uri, rtsp_config, capture_config
FROM public.devices
WHERE site_id = $1
ORDER BY id`,
		siteID,
	)
	if err != nil {
		return siteRecord{}, nil, errors.New("site load failed")
	}

	devices := make([]deviceRecord, 0)
	for rows.Next() {
		var device deviceRecord
		var gcsURI sql.NullString
		if err := rows.Scan(
			&device.id,
			&device.status,
			&gcsURI,
			&device.rtspConfig,
			&device.captureConfig,
		); err != nil {
			_ = rows.Close()
			return siteRecord{}, nil, errors.New("site load failed")
		}
		if gcsURI.Valid {
			device.gcsURI = gcsURI.String
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return siteRecord{}, nil, errors.New("site load failed")
	}
	if err := rows.Close(); err != nil {
		return siteRecord{}, nil, errors.New("site load failed")
	}
	if err := transaction.Commit(); err != nil {
		return siteRecord{}, nil, errors.New("site load failed")
	}

	return site, devices, nil
}

func (store *postgresStore) RecordEvent(ctx context.Context, event gatewayEvent) error {
	details := `{}`
	if event.stage != "" {
		encoded, err := json.Marshal(struct {
			Stage string `json:"stage"`
		}{Stage: event.stage})
		if err != nil {
			return errors.New("gateway log write failed")
		}
		details = string(encoded)
	}

	var deviceID any
	if event.deviceID != nil {
		deviceID = *event.deviceID
	}
	_, err := store.database.ExecContext(
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
		event.siteID,
		deviceID,
		event.occurredAt.UTC(),
		event.eventType,
		event.message,
		details,
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
		return errors.New("database runtime write failed")
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
		return errors.New("database runtime write failed")
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
		return errors.New("database runtime write failed")
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
		return errors.New("database runtime write failed")
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
		return errors.New("database runtime write failed")
	}
	if err := transaction.Commit(); err != nil {
		return errors.New("database runtime write failed")
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
	if databaseErr != nil || dialerErr != nil {
		return errors.New("gateway shutdown failed")
	}
	return nil
}
