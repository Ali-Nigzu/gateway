//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows/svc"
)

func TestWindowsServiceLifecycleHandoffRequestsRecovery(t *testing.T) {
	serviceSpecific, code := windowsServiceResult(controllerExitLifecycleHandoff)
	if !serviceSpecific || code == 0 {
		t.Fatalf("lifecycle handoff did not preserve SCM recovery (%t, %d)", serviceSpecific, code)
	}
	serviceSpecific, code = windowsServiceResult(controllerExitStopped)
	if !serviceSpecific || code != serviceExitControllerStopped {
		t.Fatalf("unexpected controller stop was not recoverable (%t, %d)", serviceSpecific, code)
	}
	serviceSpecific, code = windowsServiceResultForHandoff(controllerExitLifecycleHandoff, true)
	if serviceSpecific || code != 0 {
		t.Fatalf("successful removal handoff queued SCM recovery (%t, %d)", serviceSpecific, code)
	}
	serviceSpecific, code = windowsServiceResultForHandoff(controllerExitLifecycleHandoff, false)
	if !serviceSpecific || code == 0 {
		t.Fatalf("non-removal lifecycle handoff lost SCM recovery (%t, %d)", serviceSpecific, code)
	}
}

func TestWindowsLifecycleHelperRechecksRemovalPrecedence(t *testing.T) {
	for _, command := range []string{
		windowsInternalUpdateCommand,
		windowsInternalRollbackCommand,
		windowsInternalRestartCommand,
	} {
		if got := windowsLifecycleCommandWithRemovalPriority(command, true); got != windowsInternalRemoveCommand {
			t.Fatalf("committed removal did not replace %q: %q", command, got)
		}
		if got := windowsLifecycleCommandWithRemovalPriority(command, false); got != command {
			t.Fatalf("uncommitted command %q changed: %q", command, got)
		}
	}
}

func TestWindowsRemovalConversionUsesRemovalNamedHelper(t *testing.T) {
	for _, command := range []string{
		windowsInternalUpdateCommand,
		windowsInternalRollbackCommand,
		windowsInternalRestartCommand,
	} {
		if !windowsRemovalHelperNeedsNormalization(command, windowsInternalRemoveCommand) {
			t.Fatalf("%q helper did not require removal normalization", command)
		}
	}
	if windowsRemovalHelperNeedsNormalization(
		windowsInternalRemoveCommand,
		windowsInternalRemoveCommand,
	) {
		t.Fatal("removal helper was normalized a second time")
	}
	if windowsRemovalHelperNeedsNormalization(
		windowsInternalUpdateCommand,
		windowsInternalUpdateCommand,
	) {
		t.Fatal("unchanged update helper required removal normalization")
	}
}

func TestWindowsRemovalRetryTriggerWaitsThroughStopPending(t *testing.T) {
	for _, test := range []struct {
		state svc.State
		want  windowsServiceTriggerAction
	}{
		{state: svc.Running, want: windowsServiceTriggerReady},
		{state: svc.StartPending, want: windowsServiceTriggerReady},
		{state: svc.ContinuePending, want: windowsServiceTriggerReady},
		{state: svc.Stopped, want: windowsServiceTriggerStart},
		{state: svc.Paused, want: windowsServiceTriggerContinue},
		{state: svc.StopPending, want: windowsServiceTriggerWait},
		{state: svc.PausePending, want: windowsServiceTriggerWait},
	} {
		action, err := windowsServiceTriggerActionForState(test.state)
		if err != nil || action != test.want {
			t.Fatalf("state %d action = %d, err %v; want %d", test.state, action, err, test.want)
		}
	}
	if _, err := windowsServiceTriggerActionForState(svc.State(255)); err == nil {
		t.Fatal("invalid service state was accepted as a retry actor")
	}
}

func TestWindowsRemovalHelperIsIndependentCommissioningGuard(t *testing.T) {
	workDirectory := t.TempDir()
	if present, err := windowsRemovalHelperStatePresent(workDirectory); err != nil || present {
		t.Fatalf("empty work directory = present %v, err %v", present, err)
	}
	if err := os.WriteFile(filepath.Join(workDirectory, "camos-gateway-update-1.exe"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if present, err := windowsRemovalHelperStatePresent(workDirectory); err != nil || present {
		t.Fatalf("update helper was treated as removal state: present %v, err %v", present, err)
	}
	if err := os.WriteFile(filepath.Join(workDirectory, "camos-gateway-remove-2.exe"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if present, err := windowsRemovalHelperStatePresent(workDirectory); err != nil || !present {
		t.Fatalf("removal helper was not detected: present %v, err %v", present, err)
	}
}

func TestWindowsNativeRemovalContinuationRequiresExactTombstoneState(t *testing.T) {
	for _, test := range []struct {
		name            string
		nativePending   bool
		packagePending  bool
		identityPresent bool
		want            bool
	}{
		{name: "helper only", nativePending: true, want: true},
		{name: "nothing remains"},
		{name: "package remains", nativePending: true, packagePending: true},
		{name: "identity remains", nativePending: true, identityPresent: true},
		{name: "package without helper", packagePending: true},
		{name: "identity without helper", identityPresent: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := windowsNativeRemovalContinuationAllowed(
				test.nativePending,
				test.packagePending,
				test.identityPresent,
			)
			if got != test.want {
				t.Fatalf("continuation = %v, want %v", got, test.want)
			}
		})
	}
}

func TestWindowsStaleLifecycleCleanupNeverSelectsCurrentHelper(t *testing.T) {
	workDirectory := t.TempDir()
	current := filepath.Join(workDirectory, "camos-gateway-remove-42.exe")
	for _, test := range []struct {
		name string
		path string
		want bool
	}{
		{name: "current helper", path: current},
		{name: "old helper", path: filepath.Join(workDirectory, "camos-gateway-remove-41.exe"), want: true},
		{name: "partial helper", path: filepath.Join(workDirectory, "camos-gateway-remove-43.exe.installing"), want: true},
		{name: "readiness", path: filepath.Join(workDirectory, ".lifecycle-ready-43"), want: true},
		{name: "unrelated", path: filepath.Join(workDirectory, "camera-state")},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := windowsLifecycleFileIsStale(test.path, current, true); got != test.want {
				t.Fatalf("stale = %v, want %v", got, test.want)
			}
		})
	}
	if windowsLifecycleFileIsStale(current, "", false) {
		t.Fatal("unknown current executable allowed a lifecycle helper to be selected")
	}
}

func TestWindowsLifecycleHelperNamesAreNeverPIDReused(t *testing.T) {
	workDirectory := t.TempDir()
	first, err := newWindowsLifecycleHelperPath(workDirectory, "remove")
	if err != nil {
		t.Fatal(err)
	}
	second, err := newWindowsLifecycleHelperPath(workDirectory, "remove")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two lifecycle attempts reused one delayed-delete pathname")
	}
	for _, path := range []string{first, second} {
		name := filepath.Base(path)
		if !strings.HasPrefix(name, "camos-gateway-remove-") ||
			!strings.HasSuffix(name, ".exe") || strings.Contains(name, "..") {
			t.Fatalf("invalid lifecycle helper name %q", name)
		}
	}
	if _, err := newWindowsLifecycleHelperPath(workDirectory, "unknown"); err == nil {
		t.Fatal("unknown lifecycle helper kind was accepted")
	}
}
