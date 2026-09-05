//go:build windows

package main

import "testing"

func TestWindowsServiceLifecycleExitsAreClean(t *testing.T) {
	for _, exit := range []controllerExit{
		controllerExitRestarted,
		controllerExitUpdated,
		controllerExitRemoved,
	} {
		serviceSpecific, code := windowsServiceResult(exit)
		if serviceSpecific || code != 0 {
			t.Fatalf("lifecycle exit %d returned service failure (%t, %d)", exit, serviceSpecific, code)
		}
	}
	serviceSpecific, code := windowsServiceResult(controllerExitStopped)
	if !serviceSpecific || code != serviceExitControllerStopped {
		t.Fatalf("unexpected controller stop was not recoverable (%t, %d)", serviceSpecific, code)
	}
}
