package main

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func TestReconcileEngineStopsWhenNotRequired(t *testing.T) {
	controls := []gatewayControl{
		{desiredState: 0},
		{desiredState: 1, siteID: sql.NullInt64{Int64: 7, Valid: true}},
		{desiredState: 2},
	}
	for _, control := range controls {
		engine, engineCtx := completedTestEngine(7, time.Now().UTC())
		reconcileEngine(context.Background(), control, false, &engine)
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
	controls := []gatewayControl{
		{
			siteID:       sql.NullInt64{Int64: 7, Valid: true},
			desiredState: 2,
		},
		{
			siteID:             sql.NullInt64{Int64: 7, Valid: true},
			desiredState:       2,
			restartRequestedAt: sql.NullTime{Time: startedAt, Valid: true},
		},
		{
			siteID:             sql.NullInt64{Int64: 7, Valid: true},
			desiredState:       2,
			restartRequestedAt: sql.NullTime{Time: startedAt.Add(-time.Second), Valid: true},
		},
	}
	for _, control := range controls {
		engine, engineCtx := completedTestEngine(7, startedAt)
		original := engine
		reconcileEngine(context.Background(), control, false, &engine)
		if engine != original {
			t.Fatal("reconcile replaced a current engine")
		}
		if engineCtx.Err() != nil {
			t.Fatal("reconcile canceled a current engine")
		}
		stopEngine(&engine)
	}
}

func TestReconcileEngineStopsRunBeforeReplacement(t *testing.T) {
	startedAt := time.Now().UTC()
	tests := []struct {
		name         string
		control      gatewayControl
		forceRestart bool
	}{
		{
			name: "site changed",
			control: gatewayControl{
				siteID:       sql.NullInt64{Int64: 8, Valid: true},
				desiredState: 2,
			},
		},
		{
			name: "restart requested",
			control: gatewayControl{
				siteID:             sql.NullInt64{Int64: 7, Valid: true},
				desiredState:       2,
				restartRequestedAt: sql.NullTime{Time: startedAt.Add(time.Second), Valid: true},
			},
		},
		{
			name: "resume recycle",
			control: gatewayControl{
				siteID:       sql.NullInt64{Int64: 7, Valid: true},
				desiredState: 2,
			},
			forceRestart: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controllerCtx, cancelController := context.WithCancel(context.Background())
			cancelController()
			engine, engineCtx := completedTestEngine(7, startedAt)
			reconcileEngine(controllerCtx, test.control, test.forceRestart, &engine)
			if engine != nil {
				t.Fatal("replacement started after controller cancellation")
			}
			if engineCtx.Err() == nil {
				t.Fatal("old engine was not canceled before replacement")
			}
		})
	}
}

func completedTestEngine(siteID int64, startedAt time.Time) (*activeEngine, context.Context) {
	engineCtx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	done <- nil
	return &activeEngine{
		siteID:    siteID,
		startedAt: startedAt,
		cancel:    cancel,
		done:      done,
	}, engineCtx
}
