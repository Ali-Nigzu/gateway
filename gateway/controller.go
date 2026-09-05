package main

import (
	"context"
	"database/sql"
	"os"
	"slices"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

const certificateRenewalRetryInterval = 24 * time.Hour

type gatewayControl struct {
	siteID              sql.NullInt64
	desiredState        int16
	desiredVersion      int16
	restartRequestedAt  sql.NullTime
	organisationID      sql.NullInt64
	organisationEnabled sql.NullBool
	siteEnabled         sql.NullBool
}

type controllerExit uint8

const (
	controllerExitStopped controllerExit = iota
	controllerExitRestarted
	controllerExitUpdated
	controllerExitRemoved
)

type controlPriority uint8

const (
	controlPriorityPreserve controlPriority = iota
	controlPriorityPark
	controlPriorityRun
	controlPriorityUpdate
	controlPriorityRemove
)

type activeEngine struct {
	siteID    int64
	devices   []deviceRecord
	startedAt time.Time
	cancel    context.CancelFunc
	done      chan error
}

func runController(
	ctx context.Context,
	credentials *runtimeCredentials,
	resume <-chan struct{},
) controllerExit {
	if credentials == nil {
		return controllerExitStopped
	}
	var (
		store               *postgresStore
		lastControl         gatewayControl
		lastDevices         []deviceRecord
		haveControl         bool
		haveDevices         bool
		engine              *activeEngine
		lastRenewalAttempt  time.Time
		restartAfterRenewal bool
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
			return controllerExitStopped

		case <-timer.C:
			exit, finished := runControllerCycle(
				ctx,
				credentials,
				&store,
				false,
				&lastControl,
				&lastDevices,
				&haveControl,
				&haveDevices,
				&engine,
				&lastRenewalAttempt,
				&restartAfterRenewal,
			)
			if finished {
				return exit
			}
			timer.Reset(retryDelay)

		case <-resume:
			exit, finished := runControllerCycle(
				ctx,
				credentials,
				&store,
				true,
				&lastControl,
				&lastDevices,
				&haveControl,
				&haveDevices,
				&engine,
				&lastRenewalAttempt,
				&restartAfterRenewal,
			)
			if finished {
				return exit
			}
			resetControllerTimer(timer)

		case <-engineDone:
			engine.cancel()
			engine = nil
		}
	}
}

func runControllerCycle(
	ctx context.Context,
	credentials *runtimeCredentials,
	store **postgresStore,
	forceRestart bool,
	lastControl *gatewayControl,
	lastDevices *[]deviceRecord,
	haveControl *bool,
	haveDevices *bool,
	engine **activeEngine,
	lastRenewalAttempt *time.Time,
	restartAfterRenewal *bool,
) (controllerExit, bool) {
	control, fresh := pollGatewayControl(ctx, credentials, store)
	if !fresh {
		// A failed DB read can never authorize terminal removal, but renewal is
		// independent of Postgres and must keep progressing through a long DB
		// outage so the appliance does not strand itself with an expired cert.
		if exit, finished := reconcileCertificateLifecycle(
			ctx,
			credentials,
			engine,
			lastRenewalAttempt,
			restartAfterRenewal,
		); finished {
			return exit, true
		}
		if forceRestart && *haveControl && *haveDevices {
			reconcileEngine(ctx, credentials, *lastControl, *lastDevices, true, engine)
		}
		return controllerExitStopped, false
	}

	*lastControl = control
	*haveControl = true
	if controlPriorityFor(control, true) == controlPriorityRemove {
		stopEngine(engine)
		if err := beginGatewayRemoval(); err == nil {
			return controllerExitRemoved, true
		}
		return controllerExitStopped, false
	}

	completePendingCommission(ctx, *store, credentials.gatewayID)

	if controlPriorityFor(control, true) == controlPriorityUpdate {
		exit, finished, revalidated := reconcileGatewayVersion(
			ctx,
			credentials,
			*store,
			control,
			engine,
		)
		if finished {
			return exit, true
		}
		if revalidated != nil {
			control = *revalidated
			*lastControl = control
		}
	}

	if exit, finished := reconcileCertificateLifecycle(
		ctx,
		credentials,
		engine,
		lastRenewalAttempt,
		restartAfterRenewal,
	); finished {
		return exit, true
	}

	if !gatewayControlRunsCamera(control) {
		*haveDevices = false
		*lastDevices = nil
		reconcileEngine(ctx, credentials, control, nil, forceRestart, engine)
		return controllerExitStopped, false
	}

	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	devices, err := (*store).loadDevices(operationCtx, control.siteID.Int64)
	cancel()
	if err != nil {
		// A fresh control change can safely park the old site's engine, but a
		// transient device read failure must not tear down a still-valid run.
		if *engine != nil && (*engine).siteID != control.siteID.Int64 {
			stopEngine(engine)
		}
		return controllerExitStopped, false
	}
	*lastDevices = slices.Clone(devices)
	*haveDevices = true
	reconcileEngine(ctx, credentials, control, devices, forceRestart, engine)
	return controllerExitStopped, false
}

func reconcileCertificateLifecycle(
	ctx context.Context,
	credentials *runtimeCredentials,
	engine **activeEngine,
	lastRenewalAttempt *time.Time,
	restartAfterRenewal *bool,
) (controllerExit, bool) {
	if *restartAfterRenewal {
		stopEngine(engine)
		if err := beginGatewayRestart(); err == nil {
			return controllerExitRestarted, true
		}
		return controllerExitStopped, false
	}

	now := time.Now().UTC()
	if !lastRenewalAttempt.IsZero() && now.Sub(*lastRenewalAttempt) < certificateRenewalRetryInterval {
		return controllerExitStopped, false
	}
	identity, err := loadGatewayIdentity(now)
	if err != nil || !gatewayCertificateNeedsRenewal(identity.certificate, now) {
		return controllerExitStopped, false
	}

	*lastRenewalAttempt = now
	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	renewed, renewalErr := renewGatewayCertificate(operationCtx, credentials, now)
	cancel()
	if renewalErr != nil || !renewed {
		return controllerExitStopped, false
	}

	*restartAfterRenewal = true
	stopEngine(engine)
	if err := beginGatewayRestart(); err == nil {
		return controllerExitRestarted, true
	}
	return controllerExitStopped, false
}

func pollGatewayControl(
	ctx context.Context,
	credentials *runtimeCredentials,
	store **postgresStore,
) (gatewayControl, bool) {
	if credentials == nil {
		return gatewayControl{}, false
	}
	if *store == nil {
		created, err := newPostgresStore(
			ctx,
			credentials.cloudPlatformTokenSource,
			credentials.databaseLoginTokenSource,
		)
		if err != nil {
			return gatewayControl{}, false
		}
		*store = created
	}

	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	control, err := (*store).refreshGatewayControl(
		operationCtx,
		credentials.gatewayID,
		BuildVersion,
	)
	cancel()
	return control, err == nil
}

func reconcileGatewayVersion(
	ctx context.Context,
	credentials *runtimeCredentials,
	store *postgresStore,
	original gatewayControl,
	engine **activeEngine,
) (controllerExit, bool, *gatewayControl) {
	candidate, err := candidateExecutablePath()
	if err != nil {
		return controllerExitStopped, false, nil
	}
	client := oauth2.NewClient(ctx, credentials.cloudPlatformTokenSource)
	client.Timeout = cloudOperationTimeout
	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	err = downloadGatewayCandidate(operationCtx, client, original.desiredVersion, candidate)
	cancel()
	if err != nil {
		return controllerExitStopped, false, nil
	}

	operationCtx, cancel = context.WithTimeout(ctx, cloudOperationTimeout)
	revalidated, err := store.refreshGatewayControl(
		operationCtx,
		credentials.gatewayID,
		BuildVersion,
	)
	cancel()
	if err != nil {
		removeCandidate(candidate)
		return controllerExitStopped, false, nil
	}
	if controlPriorityFor(revalidated, true) == controlPriorityRemove {
		removeCandidate(candidate)
		stopEngine(engine)
		if err := beginGatewayRemoval(); err == nil {
			return controllerExitRemoved, true, &revalidated
		}
		return controllerExitStopped, false, &revalidated
	}
	if !candidateTargetStillCurrent(original.desiredVersion, revalidated, true) {
		removeCandidate(candidate)
		return controllerExitStopped, false, &revalidated
	}

	stopEngine(engine)
	if err := applyGatewayUpdate(candidate); err != nil {
		return controllerExitStopped, false, &revalidated
	}
	return controllerExitUpdated, true, &revalidated
}

func completePendingCommission(ctx context.Context, store *postgresStore, gatewayID uuid.UUID) {
	pending, err := pendingCommissionHash()
	if err != nil || pending == nil || store == nil {
		return
	}
	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	cleared, err := store.clearCommissionHash(operationCtx, gatewayID, pending[:])
	cancel()
	if err != nil {
		return
	}
	if !cleared {
		operationCtx, cancel = context.WithTimeout(ctx, cloudOperationTimeout)
		hash, exists, readErr := store.readCommissionHash(operationCtx, gatewayID)
		cancel()
		if readErr != nil || !exists || hash != nil {
			return
		}
	}
	_ = removePendingCommissionHash()
}

func gatewayControlRunsCamera(control gatewayControl) bool {
	return control.desiredState == 2 &&
		control.siteID.Valid &&
		control.organisationID.Valid &&
		control.organisationEnabled.Valid && control.organisationEnabled.Bool &&
		control.siteEnabled.Valid && control.siteEnabled.Bool
}

func controlPriorityFor(control gatewayControl, fresh bool) controlPriority {
	if !fresh {
		return controlPriorityPreserve
	}
	if control.desiredState == 0 && !control.siteID.Valid {
		return controlPriorityRemove
	}
	if control.desiredVersion > 0 && control.desiredVersion != BuildVersion {
		return controlPriorityUpdate
	}
	if gatewayControlRunsCamera(control) {
		return controlPriorityRun
	}
	return controlPriorityPark
}

func candidateTargetStillCurrent(
	downloadedVersion int16,
	revalidated gatewayControl,
	fresh bool,
) bool {
	return fresh && controlPriorityFor(revalidated, fresh) != controlPriorityRemove &&
		downloadedVersion > 0 && revalidated.desiredVersion == downloadedVersion &&
		revalidated.desiredVersion != BuildVersion
}

func removeCandidate(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + ".downloading")
}

func reconcileEngine(
	ctx context.Context,
	credentials *runtimeCredentials,
	control gatewayControl,
	devices []deviceRecord,
	forceRestart bool,
	engine **activeEngine,
) {
	if !gatewayControlRunsCamera(control) {
		stopEngine(engine)
		return
	}

	shouldStart := *engine == nil
	shouldReplace := !shouldStart && ((*engine).siteID != control.siteID.Int64 ||
		!slices.Equal((*engine).devices, devices) ||
		forceRestart ||
		(control.restartRequestedAt.Valid &&
			control.restartRequestedAt.Time.After((*engine).startedAt)))

	if shouldReplace {
		stopEngine(engine)
		shouldStart = true
	}
	if shouldStart && ctx.Err() == nil {
		*engine = startEngine(ctx, credentials, control.siteID.Int64, devices)
	}
}

func startEngine(
	ctx context.Context,
	credentials *runtimeCredentials,
	siteID int64,
	devices []deviceRecord,
) *activeEngine {
	engineCtx, cancel := context.WithCancel(ctx)
	engine := &activeEngine{
		siteID:    siteID,
		devices:   slices.Clone(devices),
		startedAt: time.Now().UTC(),
		cancel:    cancel,
		done:      make(chan error, 1),
	}
	go func() {
		engine.done <- startGateway(engineCtx, devices, credentials)
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
