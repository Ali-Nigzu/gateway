package main

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"slices"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

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
	controllerExitLifecycleHandoff
)

type controlPriority uint8

const (
	controlPriorityPark controlPriority = iota
	controlPriorityRun
	controlPriorityUpdate
	controlPriorityRemove
)

// runnableSnapshot keeps Gateway control and the device configuration read for
// that exact route together. A controller never constructs an engine from
// independently cached control and device values.
type runnableSnapshot struct {
	control gatewayControl
	devices []deviceRecord
}

type activeEngine struct {
	organisationID int64
	siteID         int64
	devices        []deviceRecord
	startedAt      time.Time
	cancel         context.CancelFunc
	done           chan error
}

type controllerRuntime struct {
	credentials              *runtimeCredentials
	store                    *postgresStore
	runnable                 *runnableSnapshot
	engine                   *activeEngine
	credentialRestartPending bool
}

func runController(
	ctx context.Context,
	credentials *runtimeCredentials,
	resume <-chan struct{},
) controllerExit {
	if credentials == nil {
		return controllerExitStopped
	}
	controller := controllerRuntime{credentials: credentials}

	timer := time.NewTimer(0)
	defer timer.Stop()
	defer controller.close()

	for {
		var engineDone <-chan error
		if controller.engine != nil {
			engineDone = controller.engine.done
		}

		forceRestart := false
		select {
		case <-ctx.Done():
			return controllerExitStopped

		case <-timer.C:
		case <-resume:
			forceRestart = true

		case <-engineDone:
			controller.engine.cancel()
			controller.engine = nil
			continue
		}

		if controller.runCycle(ctx, forceRestart) {
			return controllerExitLifecycleHandoff
		}
		if forceRestart {
			resetControllerTimer(timer)
		} else {
			timer.Reset(retryDelay)
		}
	}
}

func (controller *controllerRuntime) close() {
	stopEngine(&controller.engine)
	if controller.store != nil {
		controller.store.close()
	}
}

func (controller *controllerRuntime) runCycle(
	ctx context.Context,
	forceRestart bool,
) bool {
	control, err := controller.pollGatewayControl(ctx)
	if err != nil {
		// A failed DB read can never authorize terminal removal, but renewal is
		// independent of Postgres and must keep progressing through a long DB
		// outage so the appliance does not strand itself with an expired cert.
		if controller.reconcileCertificateLifecycle(ctx) {
			return true
		}
		if forceRestart && controller.runnable != nil {
			controller.reconcileEngine(ctx, controller.runnable, true)
		}
		return false
	}

	if controlPriorityFor(control) == controlPriorityRemove {
		return controller.beginRemoval()
	}

	completePendingCommission(ctx, controller.store, controller.credentials.gatewayID)

	if controlPriorityFor(control) == controlPriorityUpdate {
		revalidated, finished := controller.reconcileGatewayVersion(ctx, control)
		if finished {
			return true
		}
		if revalidated != nil {
			control = *revalidated
			if controlPriorityFor(control) == controlPriorityRemove {
				return controller.beginRemoval()
			}
		}
	}

	if controller.reconcileCertificateLifecycle(ctx) {
		return true
	}

	if !gatewayControlRunsCamera(control) {
		controller.park()
		return false
	}

	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	devices, err := controller.store.loadDevices(operationCtx, control.siteID.Int64)
	cancel()
	if err != nil {
		controller.preserveOnlyMatchingRoute(control)
		return false
	}

	snapshot, valid := newRunnableSnapshot(control, devices)
	if !valid {
		controller.preserveOnlyMatchingRoute(control)
		return false
	}
	controller.runnable = snapshot
	controller.reconcileEngine(ctx, snapshot, forceRestart)
	return false
}

func (controller *controllerRuntime) beginRemoval() bool {
	controller.park()
	if err := beginGatewayRemoval(); err != nil {
		return false
	}
	return true
}

type certificateRenewalFunc func(
	context.Context,
	*runtimeCredentials,
	time.Time,
) (bool, error)

func (controller *controllerRuntime) reconcileCertificateLifecycle(
	ctx context.Context,
) bool {
	return controller.reconcileCertificateLifecycleWith(
		ctx,
		time.Now().UTC(),
		renewGatewayCertificate,
		beginGatewayRestart,
	)
}

func (controller *controllerRuntime) reconcileCertificateLifecycleWith(
	ctx context.Context,
	now time.Time,
	renew certificateRenewalFunc,
	restart func() error,
) bool {
	if controller.credentialRestartPending {
		stopEngine(&controller.engine)
		return restart() == nil
	}

	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	renewed, err := renew(operationCtx, controller.credentials, now)
	cancel()
	if err != nil || !renewed {
		return false
	}

	controller.credentialRestartPending = true
	stopEngine(&controller.engine)
	return restart() == nil
}

func (controller *controllerRuntime) pollGatewayControl(
	ctx context.Context,
) (gatewayControl, error) {
	if controller.credentials == nil {
		return gatewayControl{}, errors.New("runtime credentials are unavailable")
	}
	if controller.store == nil {
		created, err := newPostgresStore(
			ctx,
			controller.credentials.cloudPlatformTokenSource,
			controller.credentials.databaseLoginTokenSource,
		)
		if err != nil {
			return gatewayControl{}, err
		}
		controller.store = created
	}

	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	control, err := controller.store.refreshGatewayControl(
		operationCtx,
		controller.credentials.gatewayID,
		BuildVersion,
	)
	cancel()
	return control, err
}

func (controller *controllerRuntime) reconcileGatewayVersion(
	ctx context.Context,
	original gatewayControl,
) (*gatewayControl, bool) {
	candidate, err := candidateExecutablePath()
	if err != nil {
		return nil, false
	}
	client := oauth2.NewClient(ctx, controller.credentials.cloudPlatformTokenSource)
	client.Timeout = cloudOperationTimeout
	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	err = downloadGatewayCandidate(operationCtx, client, original.desiredVersion, candidate)
	cancel()
	if err != nil {
		return nil, false
	}

	operationCtx, cancel = context.WithTimeout(ctx, cloudOperationTimeout)
	revalidated, err := controller.store.refreshGatewayControl(
		operationCtx,
		controller.credentials.gatewayID,
		BuildVersion,
	)
	cancel()
	if err != nil {
		removeCandidate(candidate)
		return nil, false
	}
	if !candidateTargetStillCurrent(original.desiredVersion, revalidated) {
		removeCandidate(candidate)
		return &revalidated, false
	}

	stopEngine(&controller.engine)
	if err := applyGatewayUpdate(candidate); err != nil {
		return &revalidated, false
	}
	return &revalidated, true
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

func controlPriorityFor(control gatewayControl) controlPriority {
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
) bool {
	return controlPriorityFor(revalidated) != controlPriorityRemove &&
		downloadedVersion > 0 && revalidated.desiredVersion == downloadedVersion &&
		revalidated.desiredVersion != BuildVersion
}

func removeCandidate(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + ".downloading")
}

func newRunnableSnapshot(
	control gatewayControl,
	devices []deviceRecord,
) (*runnableSnapshot, bool) {
	if !gatewayControlRunsCamera(control) {
		return nil, false
	}
	for _, device := range devices {
		if device.siteID != control.siteID.Int64 ||
			device.organisationID != control.organisationID.Int64 {
			return nil, false
		}
	}
	return &runnableSnapshot{
		control: control,
		devices: slices.Clone(devices),
	}, true
}

func gatewayControlRoutesMatch(first, second gatewayControl) bool {
	return first.siteID.Valid && second.siteID.Valid &&
		first.organisationID.Valid && second.organisationID.Valid &&
		first.siteID.Int64 == second.siteID.Int64 &&
		first.organisationID.Int64 == second.organisationID.Int64
}

func (controller *controllerRuntime) preserveOnlyMatchingRoute(control gatewayControl) {
	if controller.runnable != nil &&
		gatewayControlRoutesMatch(controller.runnable.control, control) {
		return
	}
	controller.park()
}

func (controller *controllerRuntime) park() {
	controller.runnable = nil
	stopEngine(&controller.engine)
}

func (controller *controllerRuntime) reconcileEngine(
	ctx context.Context,
	snapshot *runnableSnapshot,
	forceRestart bool,
) {
	if snapshot == nil {
		controller.park()
		return
	}

	control := snapshot.control
	shouldStart := controller.engine == nil
	shouldReplace := !shouldStart && (controller.engine.siteID != control.siteID.Int64 ||
		controller.engine.organisationID != control.organisationID.Int64 ||
		!slices.Equal(controller.engine.devices, snapshot.devices) ||
		forceRestart ||
		(control.restartRequestedAt.Valid &&
			control.restartRequestedAt.Time.After(controller.engine.startedAt)))

	if shouldReplace {
		stopEngine(&controller.engine)
		shouldStart = true
	}
	if shouldStart && ctx.Err() == nil {
		controller.engine = startEngine(ctx, controller.credentials, snapshot)
	}
}

func startEngine(
	ctx context.Context,
	credentials *runtimeCredentials,
	snapshot *runnableSnapshot,
) *activeEngine {
	engineCtx, cancel := context.WithCancel(ctx)
	engine := &activeEngine{
		organisationID: snapshot.control.organisationID.Int64,
		siteID:         snapshot.control.siteID.Int64,
		devices:        snapshot.devices,
		startedAt:      time.Now().UTC(),
		cancel:         cancel,
		done:           make(chan error, 1),
	}
	go func() {
		engine.done <- startGateway(engineCtx, snapshot.devices, credentials)
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
