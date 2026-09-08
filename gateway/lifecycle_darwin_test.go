//go:build darwin

package main

import (
	"errors"
	"strings"
	"testing"
)

func TestDarwinRemovalNativeStateIncludesLoadedJobWithoutPlist(t *testing.T) {
	if pending, err := darwinRemovalNativeState(false, true, nil); err != nil || !pending {
		t.Fatalf("loaded job without plist = pending %v, err %v", pending, err)
	}
	if pending, err := darwinRemovalNativeState(false, false, nil); err != nil || pending {
		t.Fatalf("proven absent job = pending %v, err %v", pending, err)
	}
	if _, err := darwinRemovalNativeState(false, false, errors.New("probe failed")); err == nil {
		t.Fatal("ambiguous loaded-job probe was accepted as absence")
	}
}

func TestLaunchctlAbsenceRequiresSpecificSystemDomainResult(t *testing.T) {
	target := "system/com.camos.gateway"
	if !launchctlPrintProvesAbsent("Could not find service com.camos.gateway in domain for system", target) ||
		!launchctlPrintProvesAbsent(`Bad request. Could not find service "com.camos.gateway" in domain for system`, target) {
		t.Fatal("canonical absent result was rejected")
	}
	for _, ambiguous := range []string{
		"permission denied",
		"input/output error",
		"Could not find service in user domain",
		"service unavailable in domain for system",
		"Could not find service com.other.gateway in domain for system",
		"permission denied; Could not find service com.camos.gateway in domain for system",
	} {
		if launchctlPrintProvesAbsent(ambiguous, target) {
			t.Fatalf("ambiguous launchctl failure %q was treated as absent", ambiguous)
		}
	}
}

func TestRemovalLaunchDaemonRetainsNativeFinalizerAcrossHelperUnlink(t *testing.T) {
	helper := gatewayIdentityDirectory + "/work/camos-gateway-removal"
	propertyList := removalLaunchDaemonPropertyList(helper)
	for _, required := range []string{
		"<string>/bin/sh</string>",
		"<string>-c</string>",
		"internal-remove",
		removalPendingFilename,
		gatewayIdentityPath,
		gatewayIdentityDirectory,
		removalLaunchDaemonPath,
		"/bin/launchctl bootout",
	} {
		if !strings.Contains(propertyList, required) {
			t.Fatalf("removal LaunchDaemon is missing %q", required)
		}
	}
	if strings.Index(propertyList, "internal-remove") > strings.Index(propertyList, "/bin/rm -rf") {
		t.Fatal("native finalization can delete identity before the validated helper succeeds")
	}
	if strings.Index(propertyList, removalPendingFilename) > strings.Index(propertyList, "/bin/rm -rf") {
		t.Fatal("native finalization can delete an existing identity without the committed marker")
	}
	if strings.Index(propertyList, gatewayIdentityPath) > strings.Index(propertyList, "/bin/rm -rf") {
		t.Fatal("native finalization can delete an identity before helper completion is proven")
	}
}
