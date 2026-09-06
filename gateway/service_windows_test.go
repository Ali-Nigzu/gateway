//go:build windows

package main

import "testing"

func TestWindowsServiceLifecycleExitsAreClean(t *testing.T) {
	serviceSpecific, code := windowsServiceResult(controllerExitLifecycleHandoff)
	if serviceSpecific || code != 0 {
		t.Fatalf("lifecycle handoff returned service failure (%t, %d)", serviceSpecific, code)
	}
	serviceSpecific, code = windowsServiceResult(controllerExitStopped)
	if !serviceSpecific || code != serviceExitControllerStopped {
		t.Fatalf("unexpected controller stop was not recoverable (%t, %d)", serviceSpecific, code)
	}
}
