package main

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestControlPriorityTruthTable(t *testing.T) {
	active := activeTestControl()
	tests := []struct {
		name     string
		control  gatewayControl
		fresh    bool
		expected controlPriority
	}{
		{"failed read preserves", gatewayControl{}, false, controlPriorityPreserve},
		{"state zero null removes", gatewayControl{desiredState: 0}, true, controlPriorityRemove},
		{"terminal outranks update", gatewayControl{desiredState: 0, desiredVersion: 2}, true, controlPriorityRemove},
		{"state zero assigned parks", gatewayControl{desiredState: 0, siteID: validInt64(7)}, true, controlPriorityPark},
		{"state one assigned parks", gatewayControl{desiredState: 1, siteID: validInt64(7)}, true, controlPriorityPark},
		{"state one null parks", gatewayControl{desiredState: 1}, true, controlPriorityPark},
		{"state two null parks", gatewayControl{desiredState: 2}, true, controlPriorityPark},
		{"missing hierarchy parks", gatewayControl{desiredState: 2, siteID: validInt64(7)}, true, controlPriorityPark},
		{"disabled organisation parks", disabledOrganisationControl(), true, controlPriorityPark},
		{"disabled site parks", disabledSiteControl(), true, controlPriorityPark},
		{"active hierarchy runs", active, true, controlPriorityRun},
		{"version mismatch updates", withDesiredVersion(active, 2), true, controlPriorityUpdate},
		{"reported version stays", withDesiredVersion(active, BuildVersion), true, controlPriorityRun},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if actual := controlPriorityFor(test.control, test.fresh); actual != test.expected {
				t.Fatalf("priority = %d, want %d", actual, test.expected)
			}
		})
	}
}

func TestCandidateTargetRequiresFreshUnchangedNonTerminalControl(t *testing.T) {
	active := withDesiredVersion(activeTestControl(), 2)
	if !candidateTargetStillCurrent(2, active, true) {
		t.Fatal("fresh unchanged target was rejected")
	}
	if candidateTargetStillCurrent(2, active, false) {
		t.Fatal("failed revalidation accepted a candidate")
	}
	changed := withDesiredVersion(active, 3)
	if candidateTargetStillCurrent(2, changed, true) {
		t.Fatal("stale target was accepted")
	}
	terminal := gatewayControl{desiredState: 0, desiredVersion: 2}
	if candidateTargetStillCurrent(2, terminal, true) {
		t.Fatal("terminal removal did not outrank update")
	}
}

func TestReconcileEngineStopsWhenNotRequired(t *testing.T) {
	controls := []gatewayControl{
		{desiredState: 0, siteID: validInt64(7)},
		{desiredState: 1, siteID: validInt64(7)},
		{desiredState: 2},
		disabledOrganisationControl(),
		disabledSiteControl(),
	}
	for _, control := range controls {
		engine, engineCtx := completedTestEngine(7, nil, time.Now().UTC())
		reconcileEngine(context.Background(), nil, control, nil, false, &engine)
		if engine != nil {
			t.Fatalf("desired state %d left an engine active", control.desiredState)
		}
		if engineCtx.Err() == nil {
			t.Fatalf("desired state %d did not cancel the engine", control.desiredState)
		}
	}
}

func TestReconcileEngineKeepsCurrentRun(t *testing.T) {
	startedAt := time.Now().UTC()
	devices := []deviceRecord{{id: 1, siteID: 7, organisationID: 9, enabled: true}}
	controls := []gatewayControl{
		activeTestControl(),
		withRestart(activeTestControl(), startedAt),
		withRestart(activeTestControl(), startedAt.Add(-time.Second)),
	}
	for _, control := range controls {
		engine, engineCtx := completedTestEngine(7, devices, startedAt)
		original := engine
		reconcileEngine(context.Background(), nil, control, devices, false, &engine)
		if engine != original {
			t.Fatal("reconcile replaced a current engine")
		}
		if engineCtx.Err() != nil {
			t.Fatal("reconcile canceled a current engine")
		}
		stopEngine(&engine)
	}
}

func TestReconcileEngineStopsBeforeReplacement(t *testing.T) {
	startedAt := time.Now().UTC()
	devices := []deviceRecord{{id: 1, siteID: 7, organisationID: 9, enabled: true}}
	tests := []struct {
		name         string
		control      gatewayControl
		devices      []deviceRecord
		forceRestart bool
	}{
		{"site changed", activeTestControlForSite(8), devices, false},
		{"configuration changed", activeTestControl(), []deviceRecord{{id: 2, siteID: 7, organisationID: 9, enabled: true}}, false},
		{"restart requested", withRestart(activeTestControl(), startedAt.Add(time.Second)), devices, false},
		{"resume recycle", activeTestControl(), devices, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controllerCtx, cancelController := context.WithCancel(context.Background())
			cancelController()
			engine, engineCtx := completedTestEngine(7, devices, startedAt)
			reconcileEngine(controllerCtx, nil, test.control, test.devices, test.forceRestart, &engine)
			if engine != nil {
				t.Fatal("replacement started after controller cancellation")
			}
			if engineCtx.Err() == nil {
				t.Fatal("old engine was not canceled before replacement")
			}
		})
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
	siteID int64,
	devices []deviceRecord,
	startedAt time.Time,
) (*activeEngine, context.Context) {
	engineCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	done <- nil
	return &activeEngine{
		siteID:    siteID,
		devices:   devices,
		startedAt: startedAt,
		cancel:    cancel,
		done:      done,
	}, engineCtx
}
