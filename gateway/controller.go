package main

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

type gatewayControl struct {
	siteID             sql.NullInt64
	desiredState       int16
	restartRequestedAt sql.NullTime
}

type activeEngine struct {
	siteID    int64
	startedAt time.Time
	cancel    context.CancelFunc
	done      chan error
}

func runController(ctx context.Context, gatewayID uuid.UUID, resume <-chan struct{}) {
	var (
		store       *postgresStore
		lastControl gatewayControl
		haveControl bool
		engine      *activeEngine
	)

	timer := time.NewTimer(0)
	defer timer.Stop()
	defer func() {
		stopEngine(&engine)
		if store != nil {
			store.close()
		}
	}()

	for {
		var engineDone <-chan error
		if engine != nil {
			engineDone = engine.done
		}

		select {
		case <-ctx.Done():
			return

		case <-timer.C:
			if control, ok := pollGatewayControl(ctx, gatewayID, &store); ok {
				lastControl = control
				haveControl = true
			}
			if haveControl {
				reconcileEngine(ctx, lastControl, false, &engine)
			}
			timer.Reset(retryDelay)

		case <-resume:
			if control, ok := pollGatewayControl(ctx, gatewayID, &store); ok {
				lastControl = control
				haveControl = true
			}
			if haveControl {
				reconcileEngine(ctx, lastControl, true, &engine)
			}
			resetControllerTimer(timer)

		case <-engineDone:
			engine.cancel()
			engine = nil
		}
	}
}

func pollGatewayControl(
	ctx context.Context,
	gatewayID uuid.UUID,
	store **postgresStore,
) (gatewayControl, bool) {
	if *store == nil {
		credentialsJSON, err := loadCredentialsJSON()
		if err != nil {
			return gatewayControl{}, false
		}
		authCtx := context.WithValue(
			ctx,
			oauth2.HTTPClient,
			&http.Client{Timeout: cloudOperationTimeout},
		)
		*store, err = newPostgresStore(authCtx, credentialsJSON)
		if err != nil {
			return gatewayControl{}, false
		}
	}

	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	control, err := (*store).refreshGatewayControl(operationCtx, gatewayID)
	cancel()
	return control, err == nil
}

func reconcileEngine(
	ctx context.Context,
	control gatewayControl,
	forceRestart bool,
	engine **activeEngine,
) {
	switch control.desiredState {
	case 0, 1:
		stopEngine(engine)

	case 2:
		if !control.siteID.Valid {
			stopEngine(engine)
			return
		}

		shouldStart := *engine == nil
		shouldReplace := !shouldStart && ((*engine).siteID != control.siteID.Int64 ||
			forceRestart ||
			(control.restartRequestedAt.Valid &&
				control.restartRequestedAt.Time.After((*engine).startedAt)))

		if shouldReplace {
			stopEngine(engine)
			shouldStart = true
		}
		if shouldStart && ctx.Err() == nil {
			*engine = startEngine(ctx, control.siteID.Int64)
		}
	}
}

func startEngine(ctx context.Context, siteID int64) *activeEngine {
	engineCtx, cancel := context.WithCancel(ctx)
	engine := &activeEngine{
		siteID:    siteID,
		startedAt: time.Now().UTC(),
		cancel:    cancel,
		done:      make(chan error, 1),
	}
	go func() {
		engine.done <- startGateway(engineCtx, siteID)
	}()
	return engine
}

func stopEngine(engine **activeEngine) {
	if *engine == nil {
		return
	}
	(*engine).cancel()
	<-(*engine).done
	*engine = nil
}

func resetControllerTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(retryDelay)
}
