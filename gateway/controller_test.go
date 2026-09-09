package main

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestControlPriorityTruthTable(t *testing.T) {
	active := activeTestControl()
	tests := []struct {
		name     string
		control  gatewayControl
		expected controlPriority
	}{
		{"state zero null removes", gatewayControl{desiredState: 0}, controlPriorityRemove},
		{"terminal outranks update", gatewayControl{desiredState: 0, desiredVersion: "2.0"}, controlPriorityRemove},
		{"state zero assigned parks", gatewayControl{desiredState: 0, siteID: validInt64(7)}, controlPriorityPark},
		{"state one assigned parks", gatewayControl{desiredState: 1, siteID: validInt64(7)}, controlPriorityPark},
		{"state one null parks", gatewayControl{desiredState: 1}, controlPriorityPark},
		{"state two null parks", gatewayControl{desiredState: 2}, controlPriorityPark},
		{"missing hierarchy parks", gatewayControl{desiredState: 2, siteID: validInt64(7)}, controlPriorityPark},
		{"disabled organisation parks", disabledOrganisationControl(), controlPriorityPark},
		{"disabled site parks", disabledSiteControl(), controlPriorityPark},
		{"active hierarchy runs", active, controlPriorityRun},
		{"upgrade mismatch updates", withDesiredVersion(active, "2.0"), controlPriorityUpdate},
		{"current version stays", withDesiredVersion(active, BuildVersion), controlPriorityRun},
		{"older release downgrades", withDesiredVersion(active, "0.9"), controlPriorityUpdate},
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
	active := withDesiredVersion(activeTestControl(), "2.0")
	if !candidateTargetStillCurrent("2.0", active) {
		t.Fatal("unchanged target was rejected")
	}
	changed := withDesiredVersion(active, "2.1")
	if candidateTargetStillCurrent("2.0", changed) {
		t.Fatal("stale target was accepted")
	}
	terminal := gatewayControl{desiredState: 0, desiredVersion: "2.0"}
	if candidateTargetStillCurrent("2.0", terminal) {
		t.Fatal("terminal removal did not outrank update")
	}
	if candidateTargetStillCurrent(BuildVersion, activeTestControl()) {
		t.Fatal("current build was accepted as an update target")
	}
}

func TestCandidateAttemptRejectsEveryLifecycleFingerprintChange(t *testing.T) {
	original := withDesiredVersion(activeTestControl(), "2.0")
	if !candidateAttemptStillCurrent(original, original) {
		t.Fatal("unchanged lifecycle fingerprint was rejected")
	}
	changes := map[string]func(gatewayControl) gatewayControl{
		"desired version": func(control gatewayControl) gatewayControl {
			control.desiredVersion = "2.1"
			return control
		},
		"desired state": func(control gatewayControl) gatewayControl {
			control.desiredState = 1
			return control
		},
		"site": func(control gatewayControl) gatewayControl {
			control.siteID = validInt64(8)
			return control
		},
		"organisation": func(control gatewayControl) gatewayControl {
			control.organisationID = validInt64(10)
			return control
		},
		"organisation enabled": func(control gatewayControl) gatewayControl {
			control.organisationEnabled.Bool = false
			return control
		},
		"site enabled": func(control gatewayControl) gatewayControl {
			control.siteEnabled.Bool = false
			return control
		},
		"restart request": func(control gatewayControl) gatewayControl {
			control.restartRequestedAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
			return control
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			if candidateAttemptStillCurrent(original, change(original)) {
				t.Fatal("changed lifecycle fingerprint was accepted")
			}
		})
	}
}

func TestTerminalRemovalRequiresStopThenSecondMatchingFreshRead(t *testing.T) {
	first := gatewayControl{desiredState: 0, desiredVersion: BuildVersion}
	events := []string{}
	committed, err := commitTerminalRemoval(
		context.Background(),
		first,
		func() bool {
			events = append(events, "stop")
			return true
		},
		func(context.Context) (gatewayControl, error) {
			events = append(events, "read-b")
			return first, nil
		},
		func() error {
			events = append(events, "marker")
			return nil
		},
	)
	if err != nil || !committed {
		t.Fatalf("terminal removal was not committed: committed=%v err=%v", committed, err)
	}
	if expected := []string{"stop", "read-b", "marker"}; !reflect.DeepEqual(events, expected) {
		t.Fatalf("events = %v, want %v", events, expected)
	}
}

func TestTerminalRemovalNeverCommitsAfterPreCommitFailure(t *testing.T) {
	first := gatewayControl{desiredState: 0, desiredVersion: BuildVersion}
	tests := []struct {
		name    string
		stop    bool
		second  gatewayControl
		readErr error
	}{
		{name: "camera stop timed out", stop: false, second: first},
		{name: "second read failed", stop: true, second: first, readErr: errors.New("db unavailable")},
		{name: "site assigned", stop: true, second: gatewayControl{desiredState: 0, desiredVersion: BuildVersion, siteID: validInt64(7)}},
		{name: "state changed", stop: true, second: gatewayControl{desiredState: 1, desiredVersion: BuildVersion}},
		{name: "version changed", stop: true, second: gatewayControl{desiredState: 0, desiredVersion: "2.0"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			markerWrites := 0
			committed, err := commitTerminalRemoval(
				context.Background(), first,
				func() bool { return test.stop },
				func(context.Context) (gatewayControl, error) { return test.second, test.readErr },
				func() error { markerWrites++; return nil },
			)
			if err == nil || committed || markerWrites != 0 {
				t.Fatalf("committed=%v writes=%d err=%v", committed, markerWrites, err)
			}
		})
	}
}

func TestTerminalRemovalMarkerPublicationFailureNeverCommits(t *testing.T) {
	first := gatewayControl{desiredState: 0, desiredVersion: BuildVersion}
	markerWrites := 0
	committed, err := commitTerminalRemoval(
		context.Background(),
		first,
		func() bool { return true },
		func(context.Context) (gatewayControl, error) { return first, nil },
		func() error {
			markerWrites++
			return errors.New("marker storage unavailable")
		},
	)
	if err == nil || committed || markerWrites != 1 {
		t.Fatalf("committed=%v writes=%d err=%v", committed, markerWrites, err)
	}
}

func TestCommittedRemovalRetriesOnlyLocalContinuation(t *testing.T) {
	stops := 0
	starts := 0
	failures := 0
	stop := func() bool { stops++; return true }
	begin := func() error {
		starts++
		if starts == 1 {
			return errors.New("helper unavailable")
		}
		return nil
	}
	record := func(error) { failures++ }
	if continueCommittedRemovalWith(stop, begin, record) {
		t.Fatal("failed helper unexpectedly completed handoff")
	}
	if !continueCommittedRemovalWith(stop, begin, record) {
		t.Fatal("committed removal did not retry local helper preparation")
	}
	if stops != 2 || starts != 2 || failures != 1 {
		t.Fatalf("stops=%d starts=%d failures=%d", stops, starts, failures)
	}
}

func TestStopEngineWithinIsBounded(t *testing.T) {
	canceled := false
	engine := &activeEngine{
		cancel: func() { canceled = true },
		done:   make(chan error),
	}
	started := time.Now()
	if stopEngineWithin(&engine, 5*time.Millisecond) {
		t.Fatal("stuck engine was reported stopped")
	}
	if !canceled || engine == nil {
		t.Fatal("stuck engine was not canceled and retained for later drain")
	}
	if time.Since(started) > time.Second {
		t.Fatal("bounded engine stop exceeded its test bound")
	}
}

func TestUpdateRetryBackoffIsBoundedAndGatewaySpecific(t *testing.T) {
	firstID := uuid.UUID{1}
	secondID := uuid.UUID{2}
	first := updateRetryDelay(firstID, "1.1", 1)
	if first < retryDelay || first > time.Hour {
		t.Fatalf("first retry delay = %v", first)
	}
	if capped := updateRetryDelay(firstID, "1.1", 100); capped > time.Hour {
		t.Fatalf("capped retry delay = %v", capped)
	}
	if first == updateRetryDelay(secondID, "1.1", 1) {
		t.Fatal("Gateway-specific jitter was not applied")
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

func withDesiredVersion(control gatewayControl, version string) gatewayControl {
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
