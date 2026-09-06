package main

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestControlPriorityTruthTable(t *testing.T) {
	active := activeTestControl()
	tests := []struct {
		name     string
		control  gatewayControl
		expected controlPriority
	}{
		{"state zero null removes", gatewayControl{desiredState: 0}, controlPriorityRemove},
		{"terminal outranks update", gatewayControl{desiredState: 0, desiredVersion: 3}, controlPriorityRemove},
		{"state zero assigned parks", gatewayControl{desiredState: 0, siteID: validInt64(7)}, controlPriorityPark},
		{"state one assigned parks", gatewayControl{desiredState: 1, siteID: validInt64(7)}, controlPriorityPark},
		{"state one null parks", gatewayControl{desiredState: 1}, controlPriorityPark},
		{"state two null parks", gatewayControl{desiredState: 2}, controlPriorityPark},
		{"missing hierarchy parks", gatewayControl{desiredState: 2, siteID: validInt64(7)}, controlPriorityPark},
		{"disabled organisation parks", disabledOrganisationControl(), controlPriorityPark},
		{"disabled site parks", disabledSiteControl(), controlPriorityPark},
		{"active hierarchy runs", active, controlPriorityRun},
		{"upgrade mismatch updates", withDesiredVersion(active, 3), controlPriorityUpdate},
		{"version two stays", withDesiredVersion(active, BuildVersion), controlPriorityRun},
		{"version one downgrades", withDesiredVersion(active, 1), controlPriorityUpdate},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if actual := controlPriorityFor(test.control); actual != test.expected {
				t.Fatalf("priority = %d, want %d", actual, test.expected)
			}
		})
	}
}

func TestCandidateTargetRequiresUnchangedNonTerminalControl(t *testing.T) {
	active := withDesiredVersion(activeTestControl(), 3)
	if !candidateTargetStillCurrent(3, active) {
		t.Fatal("unchanged target was rejected")
	}
	changed := withDesiredVersion(active, 4)
	if candidateTargetStillCurrent(3, changed) {
		t.Fatal("stale target was accepted")
	}
	terminal := gatewayControl{desiredState: 0, desiredVersion: 3}
	if candidateTargetStillCurrent(3, terminal) {
		t.Fatal("terminal removal did not outrank update")
	}
	if candidateTargetStillCurrent(BuildVersion, activeTestControl()) {
		t.Fatal("current build was accepted as an update target")
	}
}

func TestRunnableSnapshotRequiresOneMatchingFreshRoute(t *testing.T) {
	control := activeTestControl()
	devices := []deviceRecord{{id: 1, siteID: 7, organisationID: 9, enabled: true}}
	snapshot, ok := newRunnableSnapshot(control, devices)
	if !ok || snapshot == nil || len(snapshot.devices) != 1 {
		t.Fatalf("matching snapshot was rejected: %#v", snapshot)
	}
	devices[0].id = 2
	if snapshot.devices[0].id != 1 {
		t.Fatal("snapshot did not own an immutable device copy")
	}

	if empty, ok := newRunnableSnapshot(control, nil); !ok || empty == nil || len(empty.devices) != 0 {
		t.Fatal("valid zero-device snapshot was rejected")
	}
	for _, mismatched := range []deviceRecord{
		{id: 1, siteID: 8, organisationID: 9},
		{id: 1, siteID: 7, organisationID: 10},
	} {
		if snapshot, ok := newRunnableSnapshot(control, []deviceRecord{mismatched}); ok || snapshot != nil {
			t.Fatalf("mismatched device was accepted: %#v", mismatched)
		}
	}
	if snapshot, ok := newRunnableSnapshot(gatewayControl{desiredState: 1}, nil); ok || snapshot != nil {
		t.Fatal("parked control produced a runnable snapshot")
	}
}

func TestDeviceReadFailurePreservesOnlySameRoute(t *testing.T) {
	controller := controllerRuntime{
		runnable: &runnableSnapshot{control: activeTestControl()},
	}
	engine, engineCtx := completedTestEngine(9, 7, nil, time.Now().UTC())
	controller.engine = engine

	controller.preserveOnlyMatchingRoute(activeTestControl())
	if controller.engine == nil || controller.runnable == nil || engineCtx.Err() != nil {
		t.Fatal("same-route transient failure did not preserve the engine")
	}

	changedOrganisation := activeTestControl()
	changedOrganisation.organisationID = validInt64(10)
	controller.preserveOnlyMatchingRoute(changedOrganisation)
	if controller.engine != nil || controller.runnable != nil || engineCtx.Err() == nil {
		t.Fatal("organisation change preserved stale routing")
	}
}

func TestDeviceReadFailureStopsEngineAfterSiteChange(t *testing.T) {
	controller := controllerRuntime{
		runnable: &runnableSnapshot{control: activeTestControl()},
	}
	engine, engineCtx := completedTestEngine(9, 7, nil, time.Now().UTC())
	controller.engine = engine
	controller.preserveOnlyMatchingRoute(activeTestControlForSite(8))
	if controller.engine != nil || controller.runnable != nil || engineCtx.Err() == nil {
		t.Fatal("site change preserved stale routing")
	}
}

func TestReconcileEngineKeepsCurrentRun(t *testing.T) {
	startedAt := time.Now().UTC()
	devices := []deviceRecord{{
		id:                          1,
		siteID:                      7,
		organisationID:              9,
		enabled:                     true,
		framePackageIntervalMinutes: 15,
	}}
	controls := []gatewayControl{
		activeTestControl(),
		withRestart(activeTestControl(), startedAt),
		withRestart(activeTestControl(), startedAt.Add(-time.Second)),
	}
	for _, control := range controls {
		engine, engineCtx := completedTestEngine(9, 7, devices, startedAt)
		controller := controllerRuntime{engine: engine}
		original := controller.engine
		controller.reconcileEngine(
			context.Background(),
			&runnableSnapshot{control: control, devices: devices},
			false,
		)
		if controller.engine != original {
			t.Fatal("reconcile replaced a current engine")
		}
		if engineCtx.Err() != nil {
			t.Fatal("reconcile canceled a current engine")
		}
		stopEngine(&controller.engine)
	}
}

func TestReconcileEngineStopsBeforeReplacement(t *testing.T) {
	startedAt := time.Now().UTC()
	devices := []deviceRecord{{
		id:                          1,
		siteID:                      7,
		organisationID:              9,
		enabled:                     true,
		framePackageIntervalMinutes: 15,
	}}
	tests := []struct {
		name         string
		control      gatewayControl
		devices      []deviceRecord
		forceRestart bool
	}{
		{"site changed", activeTestControlForSite(8), devices, false},
		{"organisation changed", activeTestControlForOrganisation(10), devices, false},
		{"configuration changed", activeTestControl(), []deviceRecord{{id: 2, siteID: 7, organisationID: 9}}, false},
		{"restart requested", withRestart(activeTestControl(), startedAt.Add(time.Second)), devices, false},
		{"resume recycle", activeTestControl(), devices, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controllerCtx, cancelController := context.WithCancel(context.Background())
			cancelController()
			engine, engineCtx := completedTestEngine(9, 7, devices, startedAt)
			controller := controllerRuntime{engine: engine}
			controller.reconcileEngine(
				controllerCtx,
				&runnableSnapshot{control: test.control, devices: test.devices},
				test.forceRestart,
			)
			if controller.engine != nil {
				t.Fatal("replacement started after controller cancellation")
			}
			if engineCtx.Err() == nil {
				t.Fatal("old engine was not canceled before replacement")
			}
		})
	}
}

func TestPackageIntervalChangeReplacesEngine(t *testing.T) {
	startedAt := time.Now().UTC()
	oldDevices := []deviceRecord{{
		id:                          1,
		siteID:                      7,
		organisationID:              9,
		enabled:                     true,
		framePackageIntervalMinutes: 15,
	}}
	newDevices := append([]deviceRecord(nil), oldDevices...)
	newDevices[0].framePackageIntervalMinutes = 5
	engine, engineCtx := completedTestEngine(9, 7, oldDevices, startedAt)
	controller := controllerRuntime{engine: engine}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	controller.reconcileEngine(
		ctx,
		&runnableSnapshot{control: activeTestControl(), devices: newDevices},
		false,
	)
	if controller.engine != nil || engineCtx.Err() == nil {
		t.Fatal("package interval change did not stop the old engine")
	}
}

func TestCertificateRenewalFailureRetriesOnNextCycle(t *testing.T) {
	attempts := 0
	controller := controllerRuntime{credentials: &runtimeCredentials{}}
	renew := func(context.Context, *runtimeCredentials, time.Time) (bool, error) {
		attempts++
		return false, errors.New("temporary renewal failure")
	}
	for cycle := 0; cycle < 2; cycle++ {
		if controller.reconcileCertificateLifecycleWith(
			context.Background(),
			time.Now().UTC(),
			renew,
			func() error { return nil },
		) {
			t.Fatal("failed renewal requested lifecycle handoff")
		}
	}
	if attempts != 2 {
		t.Fatalf("renewal attempts = %d, want 2", attempts)
	}
}

func TestSuccessfulRenewalRetainsRestartUntilHandoff(t *testing.T) {
	renewals := 0
	restarts := 0
	controller := controllerRuntime{credentials: &runtimeCredentials{}}
	renew := func(context.Context, *runtimeCredentials, time.Time) (bool, error) {
		renewals++
		return true, nil
	}
	restart := func() error {
		restarts++
		if restarts == 1 {
			return errors.New("restart helper unavailable")
		}
		return nil
	}
	if controller.reconcileCertificateLifecycleWith(
		context.Background(), time.Now().UTC(), renew, restart,
	) {
		t.Fatal("failed restart unexpectedly handed off lifecycle")
	}
	if !controller.credentialRestartPending {
		t.Fatal("successful renewal did not retain restart state")
	}
	if !controller.reconcileCertificateLifecycleWith(
		context.Background(), time.Now().UTC(), renew, restart,
	) {
		t.Fatal("later restart success did not hand off lifecycle")
	}
	if renewals != 1 || restarts != 2 {
		t.Fatalf("renewals=%d restarts=%d, want 1 and 2", renewals, restarts)
	}
}

func activeTestControl() gatewayControl {
	return activeTestControlForSite(7)
}

func activeTestControlForSite(siteID int64) gatewayControl {
	return gatewayControl{
		siteID:              validInt64(siteID),
		desiredState:        2,
		desiredVersion:      BuildVersion,
		organisationID:      validInt64(9),
		organisationEnabled: sql.NullBool{Bool: true, Valid: true},
		siteEnabled:         sql.NullBool{Bool: true, Valid: true},
	}
}

func activeTestControlForOrganisation(organisationID int64) gatewayControl {
	control := activeTestControl()
	control.organisationID = validInt64(organisationID)
	return control
}

func disabledOrganisationControl() gatewayControl {
	control := activeTestControl()
	control.organisationEnabled.Bool = false
	return control
}

func disabledSiteControl() gatewayControl {
	control := activeTestControl()
	control.siteEnabled.Bool = false
	return control
}

func withDesiredVersion(control gatewayControl, version int16) gatewayControl {
	control.desiredVersion = version
	return control
}

func withRestart(control gatewayControl, at time.Time) gatewayControl {
	control.restartRequestedAt = sql.NullTime{Time: at, Valid: true}
	return control
}

func validInt64(value int64) sql.NullInt64 {
	return sql.NullInt64{Int64: value, Valid: true}
}

func completedTestEngine(
	organisationID int64,
	siteID int64,
	devices []deviceRecord,
	startedAt time.Time,
) (*activeEngine, context.Context) {
	engineCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	done <- nil
	return &activeEngine{
		organisationID: organisationID,
		siteID:         siteID,
		devices:        devices,
		startedAt:      startedAt,
		cancel:         cancel,
		done:           done,
	}, engineCtx
}
