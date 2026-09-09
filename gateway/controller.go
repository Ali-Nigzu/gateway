package main

import (
	"context"
	"database/sql"
	"errors"
	"hash/fnv"
	"os"
	"slices"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
)

type gatewayControl struct {
	siteID              sql.NullInt64
	desiredState        int16
	desiredVersion      string
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
	removalCommitted         bool
	startupUpdateFailure     error
	updateRetryTarget        string
	updateRetryFailures      uint8
	updateRetryAfter         time.Time
}

const lifecycleEngineStopTimeout = 2 * time.Minute

func runController(
	ctx context.Context,
	credentials *runtimeCredentials,
	resume <-chan struct{},
	startupUpdateFailure error,
) controllerExit {
	if credentials == nil {
		return controllerExitStopped
	}
	controller := controllerRuntime{
		credentials:          credentials,
		startupUpdateFailure: startupUpdateFailure,
	}

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
	if controller.removalCommitted {
		return controller.continueCommittedRemoval()
	}
	// Re-read local irreversible authority before consulting Postgres or
	// allowing any normal lifecycle work. A marker that became visible during
	// an ambiguous publication result must still dominate this process; an
	// unreadable or mismatched marker parks it fail-closed.
	removalPending, removalErr := gatewayRemovalPending()
	if removalErr != nil {
		controller.park()
		_ = writeLifecycleStatus(
			lifecycleStatusTerminalRemoval,
			BuildVersion,
			removalErr.Error(),
		)
		return false
	}
	if removalPending {
		controller.removalCommitted = true
		return controller.continueCommittedRemoval()
	}
	// A process that outlived terminal removal must never adopt a replacement
	// commission through the canonical path. Park it before any cloud, update,
	// renewal, or camera work unless its startup identity generation is exact.
	if err := validateRuntimeIdentityGeneration(controller.credentials); err != nil {
		controller.park()
		return false
	}
	control, err := controller.pollGatewayControl(ctx)
	if err != nil {
		if controller.startupUpdateFailure != nil {
			controller.park()
			_ = writeLifecycleStatus(
				lifecycleStatusRollback,
				BuildVersion,
				controller.startupUpdateFailure.Error(),
			)
			return false
		}
		// A failed DB read can never authorize terminal removal, but renewal is
		// independent of Postgres and must keep progressing through a long DB
		// outage so the appliance does not strand itself with an expired cert.
		if controller.reconcileCertificateLifecycle(ctx) {
			return true
		}
		if forceRestart && controller.runnable != nil &&
			validateRuntimeIdentityGeneration(controller.credentials) == nil {
			controller.reconcileEngine(ctx, controller.runnable, true)
		}
		return false
	}
	// A fresh terminal decision outranks every update state, including a
	// malformed or otherwise unrecoverable update.pending record. This check
	// must remain before both startup recovery blocking and confirmation.
	if controlPriorityFor(control) == controlPriorityRemove {
		return controller.beginRemoval(ctx, control)
	}
	// Reconcile local transition bytes before confirmation on every successful
	// fresh nonterminal control read. This retries transient pre-commit cleanup
	// failures in the live process and rolls back a candidate whose active bytes
	// became unreadable before DB confirmation. Terminal DB authority remains
	// above this block and therefore still wins over rollback.
	handoff, removalCommitted, failureCategory, retryErr := reconcileAndConfirmPendingUpdate(controller.credentials.gatewayID)
	if handoff {
		return true
	}
	if removalCommitted {
		controller.removalCommitted = true
		return controller.continueCommittedRemoval()
	}
	controller.startupUpdateFailure = retryErr
	if retryErr != nil {
		controller.park()
		_ = writeLifecycleStatus(
			failureCategory,
			BuildVersion,
			retryErr.Error(),
		)
		return false
	}
	completePendingCommission(ctx, controller.store, controller.credentials)

	if controlPriorityFor(control) == controlPriorityUpdate {
		revalidated, finished := controller.reconcileGatewayVersion(ctx, control)
		if finished {
			return true
		}
		if revalidated != nil {
			control = *revalidated
			if controlPriorityFor(control) == controlPriorityRemove {
				return controller.beginRemoval(ctx, control)
			}
		}
		if controller.removalCommitted {
			return controller.continueCommittedRemoval()
		}
		if controller.startupUpdateFailure != nil {
			return false
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
	if err := validateRuntimeIdentityGeneration(controller.credentials); err != nil {
		controller.park()
		return false
	}
	controller.runnable = snapshot
	controller.reconcileEngine(ctx, snapshot, forceRestart)
	return false
}

func (controller *controllerRuntime) beginRemoval(
	ctx context.Context,
	first gatewayControl,
) bool {
	// The first fresh terminal read authorizes only a bounded camera stop. Do
	// that before any other fallible preparation so a lock/path failure cannot
	// leave camera work running under observed terminal control.
	controller.runnable = nil
	if !stopEngineWithin(&controller.engine, lifecycleEngineStopTimeout) {
		_ = writeLifecycleStatus(
			lifecycleStatusTerminalRemoval,
			BuildVersion,
			"camera shutdown did not complete within the lifecycle bound",
		)
		return false
	}
	release, err := acquireGatewayLifecycleLock(lifecycleOperationLockWait)
	if err != nil {
		controller.park()
		_ = writeLifecycleStatus(lifecycleStatusTerminalRemoval, BuildVersion, err.Error())
		return false
	}
	defer release()
	if err := validateRuntimeIdentityGeneration(controller.credentials); err != nil {
		controller.park()
		return false
	}
	if pending, pendingErr := gatewayRemovalPending(); pendingErr != nil {
		controller.park()
		_ = writeLifecycleStatus(lifecycleStatusTerminalRemoval, BuildVersion, pendingErr.Error())
		return false
	} else if pending {
		controller.removalCommitted = true
		return controller.continueCommittedRemoval()
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		controller.park()
		_ = writeLifecycleStatus(lifecycleStatusTerminalRemoval, BuildVersion, err.Error())
		return false
	}
	committed, err := commitTerminalRemoval(
		ctx,
		first,
		func() bool { return true },
		func(readCtx context.Context) (gatewayControl, error) {
			operationCtx, cancel := context.WithTimeout(readCtx, cloudOperationTimeout)
			defer cancel()
			return controller.store.refreshGatewayControl(
				operationCtx,
				controller.credentials.gatewayID,
				BuildVersion,
			)
		},
		func() error {
			return commitGatewayRemovalMarker(paths, controller.credentials.gatewayID)
		},
	)
	if err != nil {
		_ = writeLifecycleStatus(lifecycleStatusTerminalRemoval, BuildVersion, err.Error())
		return false
	}
	if !committed {
		return false
	}
	controller.removalCommitted = true
	return controller.continueCommittedRemoval()
}

func (controller *controllerRuntime) continueCommittedRemoval() bool {
	controller.runnable = nil
	return continueCommittedRemovalWith(
		func() bool {
			return stopEngineWithin(&controller.engine, lifecycleEngineStopTimeout)
		},
		beginGatewayRemoval,
		func(err error) {
			_ = writeLifecycleStatus(lifecycleStatusTerminalRemoval, BuildVersion, err.Error())
		},
	)
}

func continueCommittedRemovalWith(
	stop terminalRemovalStop,
	begin func() error,
	recordFailure func(error),
) bool {
	if stop == nil || !stop() {
		err := errors.New("camera shutdown did not complete within the lifecycle bound")
		if recordFailure != nil {
			recordFailure(err)
		}
		return false
	}
	if begin == nil {
		err := errors.New("terminal removal continuation is unavailable")
		if recordFailure != nil {
			recordFailure(err)
		}
		return false
	}
	if err := begin(); err != nil {
		if recordFailure != nil {
			recordFailure(err)
		}
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
		if !stopEngineWithin(&controller.engine, lifecycleEngineStopTimeout) {
			return false
		}
		return restart() == nil
	}

	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	renewed, err := renew(operationCtx, controller.credentials, now)
	cancel()
	if err != nil || !renewed {
		return false
	}

	controller.credentialRestartPending = true
	if !stopEngineWithin(&controller.engine, lifecycleEngineStopTimeout) {
		return false
	}
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
	if !controller.updateAttemptAllowed(original.desiredVersion, time.Now().UTC()) {
		return &original, false
	}
	candidate, err := candidateExecutablePath()
	if err != nil {
		controller.recordUpdateFailure(original.desiredVersion, lifecycleStatusDownload, err)
		return nil, false
	}
	client := oauth2.NewClient(ctx, controller.credentials.cloudPlatformTokenSource)
	releaseDownload, err := downloadGatewayCandidate(ctx, client, original.desiredVersion, candidate)
	if err != nil {
		controller.recordUpdateFailure(
			original.desiredVersion,
			lifecycleStatusCategory(artifactFailureCategoryOf(err)),
			err,
		)
		return nil, false
	}
	defer releaseDownload()
	target, err := runtimeReleaseTarget()
	if err != nil {
		removeCandidate(candidate)
		controller.recordUpdateFailure(original.desiredVersion, lifecycleStatusCandidateIdentity, err)
		return nil, false
	}
	release, err := acquireGatewayLifecycleLock(lifecycleOperationLockWait)
	if err != nil {
		controller.startupUpdateFailure = err
		controller.park()
		controller.recordUpdateFailure(original.desiredVersion, lifecycleStatusReplacement, err)
		return nil, false
	}
	defer release()
	if err := validateRuntimeIdentityGeneration(controller.credentials); err != nil {
		controller.startupUpdateFailure = err
		controller.park()
		controller.recordUpdateFailure(
			original.desiredVersion,
			lifecycleStatusReplacement,
			err,
		)
		return nil, false
	}
	removalPending, err := gatewayRemovalPending()
	if err != nil {
		controller.startupUpdateFailure = err
		controller.park()
		controller.recordUpdateFailure(original.desiredVersion, lifecycleStatusTerminalRemoval, err)
		return nil, false
	}
	if removalPending {
		controller.removalCommitted = true
		return &original, false
	}
	identity, err := inspectGatewayCandidate(candidate, original.desiredVersion, target)
	if err != nil {
		removeCandidate(candidate)
		controller.recordUpdateFailure(original.desiredVersion, lifecycleStatusCandidateIdentity, err)
		return nil, false
	}
	pending, err := prepareUpdateCommitLocked(
		candidate,
		BuildVersion,
		original.desiredVersion,
		target,
		controller.credentials.gatewayID,
	)
	if err != nil {
		// Candidate and previous paths are process-shared. A create-only pending
		// publication may have lost a race to the authoritative service process;
		// leave deterministic cleanup to that record/startup reconciliation.
		controller.startupUpdateFailure = err
		controller.park()
		controller.recordUpdateFailure(original.desiredVersion, lifecycleStatusReplacement, err)
		return nil, false
	}
	if identity.SHA256 != pending.candidateSHA256 {
		err = errors.New("candidate hash changed before update preparation")
		controller.abortPreparedUpdateLocked(candidate, original.desiredVersion, lifecycleStatusSHA, err)
		return nil, false
	}
	controller.runnable = nil
	if !stopEngineWithin(&controller.engine, lifecycleEngineStopTimeout) {
		err = errors.New("camera shutdown did not complete within the lifecycle bound")
		controller.abortPreparedUpdateLocked(candidate, original.desiredVersion, lifecycleStatusRevalidation, err)
		return nil, false
	}
	identity, err = inspectGatewayCandidate(candidate, original.desiredVersion, target)
	if err != nil || identity.SHA256 != pending.candidateSHA256 {
		if err == nil {
			err = errors.New("candidate bytes changed before replacement")
		}
		controller.abortPreparedUpdateLocked(candidate, original.desiredVersion, lifecycleStatusCandidateIdentity, err)
		return nil, false
	}
	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	revalidated, err := controller.store.refreshGatewayControl(
		operationCtx,
		controller.credentials.gatewayID,
		BuildVersion,
	)
	cancel()
	if err != nil {
		controller.abortPreparedUpdateLocked(candidate, original.desiredVersion, lifecycleStatusRevalidation, err)
		return nil, false
	}
	if !candidateAttemptStillCurrent(original, revalidated) {
		controller.abortPreparedUpdateLocked(
			candidate,
			original.desiredVersion,
			lifecycleStatusRevalidation,
			errors.New("authoritative lifecycle control changed before replacement"),
		)
		return &revalidated, false
	}
	removalPending, err = gatewayRemovalPending()
	if err != nil {
		controller.startupUpdateFailure = err
		controller.park()
		controller.abortPreparedUpdateLocked(candidate, original.desiredVersion, lifecycleStatusTerminalRemoval, err)
		return &revalidated, false
	}
	if removalPending {
		controller.removalCommitted = true
		_ = cancelPreparedUpdateLocked(candidate)
		return &revalidated, false
	}
	outcome, err := applyGatewayUpdate(candidate)
	if outcome == gatewayReplacementPreCommit {
		if err == nil {
			err = errors.New("Gateway replacement did not cross or hand off its commit boundary")
		}
		controller.abortPreparedUpdateLocked(candidate, original.desiredVersion, lifecycleStatusReplacement, err)
		return &revalidated, false
	}
	if outcome != gatewayReplacementPostCommit && outcome != gatewayReplacementHelperHandoff {
		err = errors.New("Gateway replacement returned an invalid commit outcome")
		controller.abortPreparedUpdateLocked(candidate, original.desiredVersion, lifecycleStatusReplacement, err)
		return &revalidated, false
	}
	controller.clearUpdateFailure()
	if err != nil {
		_ = writeLifecycleStatus(lifecycleStatusReplacement, original.desiredVersion, err.Error())
	}
	return &revalidated, true
}

func (controller *controllerRuntime) abortPreparedUpdateLocked(
	candidate string,
	target string,
	category lifecycleStatusCategory,
	operationErr error,
) {
	cleanupErr := cancelPreparedUpdateLocked(candidate)
	if cleanupErr != nil {
		controller.startupUpdateFailure = cleanupErr
		controller.park()
		operationErr = errors.Join(operationErr, cleanupErr)
	}
	controller.recordUpdateFailure(target, category, operationErr)
}

func completePendingCommission(
	ctx context.Context,
	store *postgresStore,
	credentials *runtimeCredentials,
) {
	if store == nil || credentials == nil {
		return
	}
	// Snapshot the pending hash only while the running process still owns the
	// startup identity generation. The cloud calls remain outside the lock.
	release, err := acquireGatewayLifecycleLock(lifecycleOperationLockWait)
	if err != nil {
		return
	}
	if err := validateRuntimeIdentityGeneration(credentials); err != nil {
		release()
		return
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		release()
		return
	}
	pending, err := loadPendingCommissionHash(paths)
	release()
	if err != nil || pending == nil {
		return
	}
	operationCtx, cancel := context.WithTimeout(ctx, cloudOperationTimeout)
	cleared, err := store.clearCommissionHash(operationCtx, credentials.gatewayID, pending[:])
	cancel()
	if err != nil {
		return
	}
	if !cleared {
		operationCtx, cancel = context.WithTimeout(ctx, cloudOperationTimeout)
		hash, exists, readErr := store.readCommissionHash(operationCtx, credentials.gatewayID)
		cancel()
		if readErr != nil || !exists || hash != nil {
			return
		}
	}
	// The result authorizes deletion only from that exact same local generation.
	release, err = acquireGatewayLifecycleLock(lifecycleOperationLockWait)
	if err != nil {
		return
	}
	defer release()
	if err := validateRuntimeIdentityGeneration(credentials); err != nil {
		return
	}
	if removalPending, removalErr := gatewayRemovalPending(); removalErr != nil || removalPending {
		return
	}
	current, err := loadPendingCommissionHash(paths)
	if err != nil || current == nil || !current.matches(*pending) {
		return
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
	if control.desiredVersion != BuildVersion {
		if validateLifecycleVersion(control.desiredVersion) != nil {
			return controlPriorityPark
		}
		return controlPriorityUpdate
	}
	if gatewayControlRunsCamera(control) {
		return controlPriorityRun
	}
	return controlPriorityPark
}

func candidateTargetStillCurrent(
	downloadedVersion string,
	revalidated gatewayControl,
) bool {
	return controlPriorityFor(revalidated) != controlPriorityRemove &&
		validateLifecycleVersion(downloadedVersion) == nil &&
		revalidated.desiredVersion == downloadedVersion &&
		revalidated.desiredVersion != BuildVersion
}

func candidateAttemptStillCurrent(original, revalidated gatewayControl) bool {
	return candidateTargetStillCurrent(original.desiredVersion, revalidated) &&
		gatewayControlLifecycleEqual(original, revalidated)
}

func gatewayControlLifecycleEqual(first, second gatewayControl) bool {
	return first.desiredState == second.desiredState &&
		first.desiredVersion == second.desiredVersion &&
		nullInt64Equal(first.siteID, second.siteID) &&
		nullInt64Equal(first.organisationID, second.organisationID) &&
		nullBoolEqual(first.organisationEnabled, second.organisationEnabled) &&
		nullBoolEqual(first.siteEnabled, second.siteEnabled) &&
		nullTimeEqual(first.restartRequestedAt, second.restartRequestedAt)
}

func nullInt64Equal(first, second sql.NullInt64) bool {
	return first.Valid == second.Valid && (!first.Valid || first.Int64 == second.Int64)
}

func nullBoolEqual(first, second sql.NullBool) bool {
	return first.Valid == second.Valid && (!first.Valid || first.Bool == second.Bool)
}

func nullTimeEqual(first, second sql.NullTime) bool {
	return first.Valid == second.Valid && (!first.Valid || first.Time.Equal(second.Time))
}

type terminalRemovalStop func() bool
type terminalRemovalRead func(context.Context) (gatewayControl, error)
type terminalRemovalCommit func() error

// commitTerminalRemoval deliberately has one small injected operations surface
// so the destructive two-read boundary can be exhaustively fault-tested.
func commitTerminalRemoval(
	ctx context.Context,
	first gatewayControl,
	stop terminalRemovalStop,
	reread terminalRemovalRead,
	commit terminalRemovalCommit,
) (bool, error) {
	if controlPriorityFor(first) != controlPriorityRemove {
		return false, errors.New("first terminal removal authorization is invalid")
	}
	if stop == nil || reread == nil || commit == nil {
		return false, errors.New("terminal removal operations are unavailable")
	}
	if !stop() {
		return false, errors.New("camera shutdown did not complete within the lifecycle bound")
	}
	second, err := reread(ctx)
	if err != nil {
		return false, errors.New("second terminal removal authorization failed")
	}
	if controlPriorityFor(second) != controlPriorityRemove ||
		!gatewayControlLifecycleEqual(first, second) {
		return false, errors.New("terminal removal authorization changed before commit")
	}
	if err := commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (controller *controllerRuntime) updateAttemptAllowed(target string, now time.Time) bool {
	if controller.updateRetryTarget != target {
		controller.updateRetryTarget = target
		controller.updateRetryFailures = 0
		controller.updateRetryAfter = time.Time{}
		return true
	}
	return controller.updateRetryAfter.IsZero() || !now.Before(controller.updateRetryAfter)
}

func (controller *controllerRuntime) recordUpdateFailure(
	target string,
	category lifecycleStatusCategory,
	err error,
) {
	if target == "" {
		target = BuildVersion
	}
	if !validLifecycleStatusCategory(category) {
		category = lifecycleStatusDownload
	}
	if err != nil {
		_ = writeLifecycleStatus(category, target, err.Error())
	}
	if controller.updateRetryTarget != target {
		controller.updateRetryTarget = target
		controller.updateRetryFailures = 0
	}
	if controller.updateRetryFailures < 8 {
		controller.updateRetryFailures++
	}
	controller.updateRetryAfter = time.Now().UTC().Add(updateRetryDelay(
		controller.credentials.gatewayID,
		target,
		controller.updateRetryFailures,
	))
}

func (controller *controllerRuntime) clearUpdateFailure() {
	controller.updateRetryTarget = ""
	controller.updateRetryFailures = 0
	controller.updateRetryAfter = time.Time{}
}

func updateRetryDelay(gatewayID uuid.UUID, target string, failures uint8) time.Duration {
	if failures == 0 {
		return 0
	}
	exponent := failures - 1
	if exponent > 5 {
		exponent = 5
	}
	base := retryDelay * time.Duration(uint64(1)<<exponent)
	maximum := time.Hour
	if base > maximum {
		base = maximum
	}
	hash := fnv.New64a()
	_, _ = hash.Write(gatewayID[:])
	_, _ = hash.Write([]byte(target))
	jitterRange := base / 5
	if jitterRange <= 0 {
		return base
	}
	jitter := time.Duration(hash.Sum64() % uint64(jitterRange))
	if base+jitter > maximum {
		return maximum
	}
	return base + jitter
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
		engine.done <- runGatewayApplication(engineCtx, snapshot.devices, credentials)
	}()
	return engine
}

func stopEngine(engine **activeEngine) {
	_ = stopEngineWithin(engine, lifecycleEngineStopTimeout)
}

func stopEngineWithin(engine **activeEngine, timeout time.Duration) bool {
	if *engine == nil {
		return true
	}
	(*engine).cancel()
	if timeout <= 0 {
		return false
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-(*engine).done:
		*engine = nil
		return true
	case <-timer.C:
		return false
	}
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
