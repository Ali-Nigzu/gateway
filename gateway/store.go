package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
)

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

	return &postgresStore{
		database: stdlib.OpenDB(*config),
		dialer:   dialer,
	}, nil
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

func runtimeFactStatement(deviceCount int) string {
	if deviceCount == 0 {
		return `UPDATE public.sites
SET gateway_last_seen_at = GREATEST(
    COALESCE(gateway_last_seen_at, CURRENT_TIMESTAMP),
    CURRENT_TIMESTAMP
)
WHERE id = $1`
	}

	var statement strings.Builder
	statement.WriteString(`WITH current_facts(device_id, connected_at, seen_at, uploaded_at) AS (
    VALUES
`)
	for index := 0; index < deviceCount; index++ {
		if index != 0 {
			statement.WriteString(",\n")
		}
		firstParameter := 2 + index*4
		fmt.Fprintf(
			&statement,
			"        ($%d::bigint, $%d::timestamptz, $%d::timestamptz, $%d::timestamptz)",
			firstParameter,
			firstParameter+1,
			firstParameter+2,
			firstParameter+3,
		)
	}
	statement.WriteString(`
),
updated_devices AS (
    UPDATE public.devices AS d
    SET
        last_connected_at = CASE
            WHEN f.connected_at IS NULL THEN d.last_connected_at
            ELSE GREATEST(COALESCE(d.last_connected_at, f.connected_at), f.connected_at)
        END,
        last_frame_seen_at = CASE
            WHEN f.seen_at IS NULL THEN d.last_frame_seen_at
            ELSE GREATEST(COALESCE(d.last_frame_seen_at, f.seen_at), f.seen_at)
        END,
        last_frame_uploaded_at = CASE
            WHEN f.uploaded_at IS NULL THEN d.last_frame_uploaded_at
            ELSE GREATEST(COALESCE(d.last_frame_uploaded_at, f.uploaded_at), f.uploaded_at)
        END
    FROM current_facts AS f
    WHERE d.id = f.device_id
      AND d.site_id = $1
      AND (
          (f.connected_at IS NOT NULL AND (d.last_connected_at IS NULL OR d.last_connected_at < f.connected_at))
          OR (f.seen_at IS NOT NULL AND (d.last_frame_seen_at IS NULL OR d.last_frame_seen_at < f.seen_at))
          OR (f.uploaded_at IS NOT NULL AND (d.last_frame_uploaded_at IS NULL OR d.last_frame_uploaded_at < f.uploaded_at))
      )
)
UPDATE public.sites
SET gateway_last_seen_at = GREATEST(
    COALESCE(gateway_last_seen_at, CURRENT_TIMESTAMP),
    CURRENT_TIMESTAMP
)
WHERE id = $1`)
	return statement.String()
}

func (store *postgresStore) writeRuntimeFacts(
	ctx context.Context,
	statement string,
	arguments []any,
) error {
	if _, err := store.database.ExecContext(ctx, statement, arguments...); err != nil {
		return errors.New("database runtime write failed")
	}
	return nil
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func (store *postgresStore) close() {
	_ = store.database.Close()
	_ = store.dialer.Close()
}
