//go:build linux

package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestLinuxRemovalNativeStateIncludesLoadedUnitWithoutFile(t *testing.T) {
	if pending, err := linuxRemovalNativeState(false, true, nil); err != nil || !pending {
		t.Fatalf("loaded unit without file = pending %v, err %v", pending, err)
	}
	if pending, err := linuxRemovalNativeState(false, false, nil); err != nil || pending {
		t.Fatalf("proven absent unit = pending %v, err %v", pending, err)
	}
	if _, err := linuxRemovalNativeState(false, false, errors.New("probe failed")); err == nil {
		t.Fatal("ambiguous loaded-unit probe was accepted as absence")
	}
}

func TestSystemdLoadStateRequiresExplicitNotFound(t *testing.T) {
	missing, err := systemdLoadStateNotFound("not-found")
	if err != nil || !missing {
		t.Fatalf("not-found state was rejected: missing=%v err=%v", missing, err)
	}
	for _, state := range []string{"loaded", "masked", "bad-setting", "error", "merged"} {
		missing, err := systemdLoadStateNotFound(state)
		if err != nil || missing {
			t.Fatalf("state %q was treated as absent: missing=%v err=%v", state, missing, err)
		}
	}
	if missing, err := systemdLoadStateNotFound("offline"); err == nil || missing {
		t.Fatalf("ambiguous state was accepted: missing=%v err=%v", missing, err)
	}
}

func TestSystemdRemovalUnitRetainsNativeFinalizerAcrossHelperUnlink(t *testing.T) {
	helper := "/var/lib/camos-gateway/work/camos-gateway-removal"
	unit, err := systemdRemovalUnit(helper)
	if err != nil {
		t.Fatal(err)
	}
	tombstone := gatewayIdentityDirectory + ".removing"
	for _, required := range []string{
		"ExecStart=/bin/sh -c",
		helper,
		"internal-remove",
		gatewayIdentityDirectory,
		filepath.Join(gatewayIdentityDirectory, removalPendingFilename),
		tombstone,
		"/bin/rm -rf --",
		systemdRemovalUnitPath,
		"/bin/systemctl daemon-reload",
	} {
		if !strings.Contains(unit, required) {
			t.Fatalf("removal unit is missing %q", required)
		}
	}
	if strings.Index(unit, "internal-remove") > strings.Index(unit, "/bin/rm -rf --") {
		t.Fatal("native finalization can delete identity before the validated helper succeeds")
	}
	if strings.Index(unit, removalPendingFilename) > strings.Index(unit, "/bin/rm -rf --") {
		t.Fatal("native finalization can delete an existing identity without the committed marker")
	}
	if strings.Index(unit, tombstone) > strings.Index(unit, "/bin/rm -rf --") {
		t.Fatal("native finalization does not bind recursive cleanup to the removal tombstone")
	}
}
