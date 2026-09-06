package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"cloud.google.com/go/cloudsqlconn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"golang.org/x/oauth2"
)

const (
	cloudSQLInstance = "camosbase:europe-west2:camos-prod-postgres"
	cloudSQLDatabase = "camos_prod"

	loadDevicesStatement = `SELECT
    d.id,
    s.organisation_id,
    d.site_id,
    d.enabled,
    d.rtsp_uri,
    d.rtsp_username,
    d.rtsp_password,
    d.capture_fps,
    d.frame_package_interval_minutes,
    d.change_threshold_bp,
    d.line_ax,
    d.line_ay,
    d.line_bx,
    d.line_by
FROM public.devices AS d
JOIN public.sites AS s
  ON s.id = d.site_id
WHERE d.site_id = $1
ORDER BY d.id`

	refreshGatewayControlStatement = `WITH refreshed_gateway AS (
    UPDATE public.gateways
    SET
        last_seen_at = GREATEST(
            COALESCE(last_seen_at, CURRENT_TIMESTAMP),
            CURRENT_TIMESTAMP
        ),
        reported_version = $2
    WHERE gateway_id = $1::uuid
    RETURNING site_id, desired_state, desired_version, restart_requested_at
)
SELECT
    g.site_id,
    g.desired_state,
    g.desired_version,
    g.restart_requested_at,
    s.organisation_id,
    o.enabled,
    s.enabled
FROM refreshed_gateway AS g
LEFT JOIN public.sites AS s
  ON s.id = g.site_id
LEFT JOIN public.organisations AS o
  ON o.id = s.organisation_id`

	clearCommissionHashStatement = `UPDATE public.gateways
SET commission_hash = NULL
WHERE gateway_id = $1::uuid
  AND commission_hash = $2::bytea`

	readCommissionHashStatement = `SELECT commission_hash
FROM public.gateways
WHERE gateway_id = $1::uuid`
)

type deviceRecord struct {
	id                          int64
	organisationID              int64
	siteID                      int64
	enabled                     bool
	rtspURI                     string
	rtspUsername                string
	rtspPassword                string
	captureFPS                  int
	framePackageIntervalMinutes int
	changeThresholdPercent      float64
	lineAX                      sql.NullInt16
	lineAY                      sql.NullInt16
	lineBX                      sql.NullInt16
	lineBY                      sql.NullInt16
}

type databaseDeviceRecord struct {
	id                          int64
	organisationID              int64
	siteID                      int64
	enabled                     bool
	rtspURI                     sql.NullString
	rtspUsername                sql.NullString
	rtspPassword                sql.NullString
	captureFPS                  int
	framePackageIntervalMinutes int
	changeThresholdBP           int32
	lineAX                      sql.NullInt16
	lineAY                      sql.NullInt16
	lineBX                      sql.NullInt16
	lineBY                      sql.NullInt16
}

type rowScanner interface {
	Scan(destinations ...any) error
}

type postgresStore struct {
	database *sql.DB
	dialer   *cloudsqlconn.Dialer
}

func newPostgresStore(
	ctx context.Context,
	apiTokenSource oauth2.TokenSource,
	databaseLoginTokenSource oauth2.TokenSource,
) (*postgresStore, error) {
	if apiTokenSource == nil || databaseLoginTokenSource == nil {
		return nil, errors.New("database startup failed")
	}
	dialer, err := cloudsqlconn.NewDialer(
		ctx,
		cloudsqlconn.WithIAMAuthN(),
		cloudsqlconn.WithIAMAuthNTokenSources(
			apiTokenSource,
			databaseLoginTokenSource,
		),
	)
	if err != nil {
		return nil, errors.New("database startup failed")
	}

	config, err := pgx.ParseConfig("sslmode=disable")
	if err != nil {
		_ = dialer.Close()
		return nil, errors.New("database startup failed")
	}
	config.User = runtimeDatabaseUser
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
		loadDevicesStatement,
		siteID,
	)
	if err != nil {
		return nil, errors.New("site load failed")
	}
	defer rows.Close()

	var devices []deviceRecord
	for rows.Next() {
		device, err := scanDeviceRecord(rows)
		if err != nil {
			return nil, errors.New("site load failed")
		}
		if device.enabled {
			devices = append(devices, device)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, errors.New("site load failed")
	}
	return devices, nil
}

func scanDeviceRecord(scanner rowScanner) (deviceRecord, error) {
	var databaseRecord databaseDeviceRecord
	if err := scanner.Scan(
		&databaseRecord.id,
		&databaseRecord.organisationID,
		&databaseRecord.siteID,
		&databaseRecord.enabled,
		&databaseRecord.rtspURI,
		&databaseRecord.rtspUsername,
		&databaseRecord.rtspPassword,
		&databaseRecord.captureFPS,
		&databaseRecord.framePackageIntervalMinutes,
		&databaseRecord.changeThresholdBP,
		&databaseRecord.lineAX,
		&databaseRecord.lineAY,
		&databaseRecord.lineBX,
		&databaseRecord.lineBY,
	); err != nil {
		return deviceRecord{}, err
	}
	if databaseRecord.framePackageIntervalMinutes <= 0 {
		return deviceRecord{}, errors.New("invalid frame package interval")
	}
	return databaseRecord.runtimeRecord(), nil
}

func (record databaseDeviceRecord) runtimeRecord() deviceRecord {
	return deviceRecord{
		id:                          record.id,
		organisationID:              record.organisationID,
		siteID:                      record.siteID,
		enabled:                     record.enabled,
		rtspURI:                     nullableString(record.rtspURI),
		rtspUsername:                nullableString(record.rtspUsername),
		rtspPassword:                nullableString(record.rtspPassword),
		captureFPS:                  record.captureFPS,
		framePackageIntervalMinutes: record.framePackageIntervalMinutes,
		changeThresholdPercent:      float64(record.changeThresholdBP) / 100,
		lineAX:                      record.lineAX,
		lineAY:                      record.lineAY,
		lineBX:                      record.lineBX,
		lineBY:                      record.lineBY,
	}
}

func nullableString(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func (store *postgresStore) refreshGatewayControl(
	ctx context.Context,
	gatewayID uuid.UUID,
	reportedVersion int16,
) (gatewayControl, error) {
	var control gatewayControl
	err := store.database.QueryRowContext(
		ctx,
		refreshGatewayControlStatement,
		gatewayID,
		reportedVersion,
	).Scan(
		&control.siteID,
		&control.desiredState,
		&control.desiredVersion,
		&control.restartRequestedAt,
		&control.organisationID,
		&control.organisationEnabled,
		&control.siteEnabled,
	)
	if err != nil {
		return gatewayControl{}, errors.New("gateway control failed")
	}
	return control, nil
}

func (store *postgresStore) clearCommissionHash(
	ctx context.Context,
	gatewayID uuid.UUID,
	expectedHash []byte,
) (bool, error) {
	if len(expectedHash) != sha256.Size {
		return false, errors.New("commission completion failed")
	}
	result, err := store.database.ExecContext(
		ctx,
		clearCommissionHashStatement,
		gatewayID,
		expectedHash,
	)
	if err != nil {
		return false, errors.New("commission completion failed")
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, errors.New("commission completion failed")
	}
	return affected == 1, nil
}

func (store *postgresStore) readCommissionHash(
	ctx context.Context,
	gatewayID uuid.UUID,
) (hash []byte, gatewayExists bool, err error) {
	err = store.database.QueryRowContext(
		ctx,
		readCommissionHashStatement,
		gatewayID,
	).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, errors.New("commission completion failed")
	}
	return hash, true, nil
}

func runtimeFactStatement(deviceCount int) string {
	if deviceCount == 0 {
		return ""
	}

	var statement strings.Builder
	statement.WriteString(`WITH current_facts(device_id, connected_at, seen_at, uploaded_at) AS (
    VALUES
`)
	for index := 0; index < deviceCount; index++ {
		if index != 0 {
			statement.WriteString(",\n")
		}
		firstParameter := 1 + index*4
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
)
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
  AND (
      (f.connected_at IS NOT NULL AND (d.last_connected_at IS NULL OR d.last_connected_at < f.connected_at))
      OR (f.seen_at IS NOT NULL AND (d.last_frame_seen_at IS NULL OR d.last_frame_seen_at < f.seen_at))
      OR (f.uploaded_at IS NOT NULL AND (d.last_frame_uploaded_at IS NULL OR d.last_frame_uploaded_at < f.uploaded_at))
  )`)
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
