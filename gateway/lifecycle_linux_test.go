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
	unit := systemdRemovalUnit(helper)
	for _, required := range []string{
		"ExecStart=/bin/sh -c",
		helper + " internal-remove",
		"if [ -e " + gatewayIdentityDirectory + " ] || [ -L " + gatewayIdentityDirectory + " ]",
		"test -f " + filepath.Join(gatewayIdentityDirectory, removalPendingFilename),
		"test ! -e " + gatewayIdentityPath,
		"test ! -L " + helper,
		"/bin/rm -rf -- " + gatewayIdentityDirectory,
		"/bin/rm -f -- " + systemdRemovalUnitPath,
		"/bin/systemctl daemon-reload",
	} {
		if !strings.Contains(unit, required) {
			t.Fatalf("removal unit is missing %q", required)
		}
	}
	if strings.Index(unit, helper+" internal-remove") > strings.Index(unit, "/bin/rm -rf -- "+gatewayIdentityDirectory) {
		t.Fatal("native finalization can delete identity before the validated helper succeeds")
	}
	if strings.Index(unit, "test -f "+filepath.Join(gatewayIdentityDirectory, removalPendingFilename)) > strings.Index(unit, "/bin/rm -rf -- "+gatewayIdentityDirectory) {
		t.Fatal("native finalization can delete an existing identity without the committed marker")
	}
	if strings.Index(unit, "test ! -e "+gatewayIdentityPath) > strings.Index(unit, "/bin/rm -rf -- "+gatewayIdentityDirectory) ||
		strings.Index(unit, "test ! -L "+helper) > strings.Index(unit, "/bin/rm -rf -- "+gatewayIdentityDirectory) {
		t.Fatal("native finalization can delete an identity before helper completion is proven")
	}
}
