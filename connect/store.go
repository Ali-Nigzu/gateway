package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

const (
	cloudSQLInstance = "camosbase:europe-west2:camos-prod-postgres"
	cloudSQLDatabase = "camos_prod"

	eventGatewayStarted = "gateway.started"
	eventDeviceStarted  = "device.started"
	eventDeviceFailed   = "device.failed"
	eventDeviceStopped  = "device.stopped"
	eventGatewayStopped = "gateway.stopped"
)

type deviceFailure string

const (
	failureStageStream   deviceFailure = "stream"
	failureStageUpload   deviceFailure = "upload"
	failureStageDatabase deviceFailure = "database"
	failureStageCrash    deviceFailure = "crash"
)

func (deviceFailure) Error() string {
	return "camera failed"
}

type deviceRecord struct {
	id            int64
	gcsURI        string
	rtspConfig    []byte
	captureConfig []byte
}

type postgresStore struct {
	database *sql.DB
	dialer   *cloudsqlconn.Dialer
}

func newPostgresStore(ctx context.Context, credentialsJSON []byte) (*postgresStore, error) {
	var credentials struct {
		ClientEmail string `json:"client_email"`
	}
	if err := json.Unmarshal(credentialsJSON, &credentials); err != nil {
		return nil, errors.New("sa.json unavailable")
	}
	databaseUser := strings.TrimSuffix(strings.TrimSpace(credentials.ClientEmail), ".gserviceaccount.com")

	dialer, err := cloudsqlconn.NewDialer(
		ctx,
		cloudsqlconn.WithCredentialsJSON(credentialsJSON),
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
	if err := database.PingContext(ctx); err != nil {
		_ = database.Close()
		_ = dialer.Close()
		return nil, errors.New("database startup failed")
	}

	return &postgresStore{database: database, dialer: dialer}, nil
}

func (store *postgresStore) loadDevices(ctx context.Context, siteID int64) ([]deviceRecord, error) {
	rows, err := store.database.QueryContext(
		ctx,
		`SELECT d.id, d.gcs_source_uri, d.rtsp_config, d.capture_config
FROM public.sites AS s
JOIN public.devices AS d
  ON d.site_id = s.id
WHERE s.id = $1
  AND s.status = 'enabled'
  AND d.status = 'enabled'
ORDER BY d.id`,
		siteID,
	)
	if err != nil {
		return nil, errors.New("site load failed")
	}
	defer rows.Close()

	var devices []deviceRecord
	for rows.Next() {
		var device deviceRecord
		if err := rows.Scan(
			&device.id,
			&device.gcsURI,
			&device.rtspConfig,
			&device.captureConfig,
		); err != nil {
			return nil, errors.New("site load failed")
		}
		devices = append(devices, device)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("site load failed")
	}
	return devices, nil
}

func (store *postgresStore) markConnected(ctx context.Context, siteID, deviceID int64, observedAt time.Time) error {
	_, err := store.database.ExecContext(
		ctx,
		`UPDATE public.devices
SET
    last_connected_at = GREATEST(COALESCE(last_connected_at, $3), $3),
    last_frame_seen_at = GREATEST(COALESCE(last_frame_seen_at, $3), $3)
WHERE id = $1
  AND site_id = $2`,
		deviceID,
		siteID,
		observedAt,
	)
	if err != nil {
		return errors.New("database runtime write failed")
	}
	return nil
}

func (store *postgresStore) markFrameSeen(ctx context.Context, siteID, deviceID int64, observedAt time.Time) error {
	_, err := store.database.ExecContext(
		ctx,
		`UPDATE public.devices
SET last_frame_seen_at = GREATEST(COALESCE(last_frame_seen_at, $3), $3)
WHERE id = $1
  AND site_id = $2`,
		deviceID,
		siteID,
		observedAt,
	)
	if err != nil {
		return errors.New("database runtime write failed")
	}
	return nil
}

func (store *postgresStore) markUploaded(
	ctx context.Context,
	siteID, deviceID int64,
	capturedAt, completedAt time.Time,
) error {
	_, err := store.database.ExecContext(
		ctx,
		`WITH updated_device AS (
    UPDATE public.devices
    SET
        last_frame_seen_at = GREATEST(COALESCE(last_frame_seen_at, $3), $3),
        last_frame_uploaded_at = GREATEST(COALESCE(last_frame_uploaded_at, $4), $4)
    WHERE id = $1
      AND site_id = $2
    RETURNING 1
)
UPDATE public.sites
SET gateway_last_seen_at = GREATEST(COALESCE(gateway_last_seen_at, $4), $4)
WHERE id = $2
  AND EXISTS (SELECT 1 FROM updated_device)`,
		deviceID,
		siteID,
		capturedAt,
		completedAt,
	)
	if err != nil {
		return errors.New("database runtime write failed")
	}
	return nil
}

func (store *postgresStore) close() error {
	databaseErr := store.database.Close()
	dialerErr := store.dialer.Close()
	if databaseErr != nil || dialerErr != nil {
		return errors.New("gateway shutdown failed")
	}
	return nil
}
