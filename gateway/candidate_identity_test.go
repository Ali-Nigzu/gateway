package main

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"
)

func TestInspectGatewayCandidateAcceptsCurrentGatewayBinary(t *testing.T) {
	executable := buildCurrentGatewayBinary(t)
	target, err := artifactPlatformFilename(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	identity, err := inspectGatewayCandidate(executable, BuildVersion, target)
	if err != nil {
		t.Fatal(err)
	}
	if identity.Version != BuildVersion || identity.Target != target ||
		identity.GOOS != runtime.GOOS || identity.GOARCH != runtime.GOARCH ||
		identity.Module != gatewayGoModule || identity.Size <= 0 {
		t.Fatalf("candidate identity = %#v", identity)
	}
	if _, err := inspectGatewayCandidate(executable, "9.9", target); err == nil {
		t.Fatal("candidate carrying a different BuildVersion stamp was accepted")
	}

	encoded, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	stamp := []byte(expectedGatewayReleaseStamp(BuildVersion, target))
	offset := bytes.Index(encoded, stamp)
	if offset < 0 || bytes.Index(encoded[offset+len(stamp):], stamp) >= 0 {
		t.Fatal("test binary does not contain one canonical release stamp")
	}
	wrongArchitecture := bytes.Clone(encoded)
	if mutateExecutableArchitecture(wrongArchitecture) {
		wrongArchitecturePath := filepath.Join(t.TempDir(), target)
		if err := os.WriteFile(wrongArchitecturePath, wrongArchitecture, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := inspectGatewayCandidate(wrongArchitecturePath, BuildVersion, target); err == nil {
			t.Fatal("candidate with the wrong GOARCH executable header was accepted")
		}
	}
	encoded[offset+len(gatewayReleaseStampPrefix)+len("module=camos-gateway|version=")] = 'X'
	mutated := filepath.Join(t.TempDir(), target)
	if err := os.WriteFile(mutated, encoded, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectGatewayCandidate(mutated, BuildVersion, target); err == nil {
		t.Fatal("candidate with a malformed release stamp was accepted")
	}
}

func TestInspectGatewayCandidateRejectsWrongExecutableFamily(t *testing.T) {
	target, err := artifactPlatformFilename(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	path := filepath.Join(t.TempDir(), target)
	if err := os.WriteFile(path, []byte("not an executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectGatewayCandidate(path, BuildVersion, target); err == nil {
		t.Fatal("wrong executable family was accepted")
	}
}

func TestInspectGatewayCandidateRejectsWrongPlatform(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrongTarget := "linux-amd64"
	if runtime.GOOS == "linux" {
		wrongTarget = "windows-amd64.exe"
	}
	if _, err := inspectGatewayCandidate(executable, BuildVersion, wrongTarget); err == nil {
		t.Fatal("candidate for a different GOOS was accepted")
	}
}

func TestInspectGatewayCandidateRejectsWrongGoModule(t *testing.T) {
	target, err := artifactPlatformFilename(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), []byte("module wrong-gateway\n\ngo 1.25\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(directory, target)
	goExecutable := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		goExecutable += ".exe"
	}
	command := exec.Command(goExecutable, "build", "-o", output, ".")
	command.Dir = directory
	command.Env = append(os.Environ(), "GOWORK=off", "GOCACHE="+filepath.Join(directory, "cache"))
	if result, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build wrong-module candidate: %v\n%s", err, result)
	}
	if _, err := inspectGatewayCandidate(output, BuildVersion, target); err == nil {
		t.Fatal("candidate from the wrong Go module was accepted")
	}
}

func TestUniqueBuildSettingRejectsMissingAndAmbiguousPlatform(t *testing.T) {
	if _, err := uniqueBuildSetting(&debug.BuildInfo{}, "GOOS"); err == nil {
		t.Fatal("missing GOOS was accepted")
	}
	info := &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "GOOS", Value: "windows"}, {Key: "GOOS", Value: "linux"}}}
	if _, err := uniqueBuildSetting(info, "GOOS"); err == nil {
		t.Fatal("ambiguous GOOS was accepted")
	}
}

func TestBoundedStampScannerFindsBoundaryAndDuplicates(t *testing.T) {
	wanted := []byte("fixed-stamp")
	scanner := newBoundedStampScanner(wanted)
	for _, chunk := range [][]byte{[]byte("prefix-fixed"), []byte("-stamp-middle-fixed-"), []byte("stamp-suffix")} {
		if _, err := scanner.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if scanner.matches != 2 {
		t.Fatalf("stamp matches = %d, want 2", scanner.matches)
	}
}

func buildCurrentGatewayBinary(t *testing.T) string {
	t.Helper()
	target, err := artifactPlatformFilename(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Skip(err)
	}
	output := filepath.Join(t.TempDir(), target)
	goExecutable := filepath.Join(runtime.GOROOT(), "bin", "go")
	if runtime.GOOS == "windows" {
		goExecutable += ".exe"
	}
	command := exec.Command(goExecutable, "build", "-o", output, ".")
	command.Env = append(os.Environ(), "GOWORK=off")
	if result, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build current Gateway candidate: %v\n%s", err, result)
	}
	return output
}

func mutateExecutableArchitecture(encoded []byte) bool {
	switch runtime.GOOS {
	case "windows":
		if len(encoded) < 0x40 {
			return false
		}
		header := int(binary.LittleEndian.Uint32(encoded[0x3c:]))
		if header < 0 || header+6 > len(encoded) {
			return false
		}
		binary.LittleEndian.PutUint16(encoded[header+4:], pe.IMAGE_FILE_MACHINE_ARM64)
	case "linux":
		if len(encoded) < 20 {
			return false
		}
		binary.LittleEndian.PutUint16(encoded[18:], uint16(elf.EM_AARCH64))
	case "darwin":
		if len(encoded) < 8 {
			return false
		}
		cpu := macho.CpuArm64
		if runtime.GOARCH == "arm64" {
			cpu = macho.CpuAmd64
		}
		binary.LittleEndian.PutUint32(encoded[4:], uint32(cpu))
	default:
		return false
	}
	return true
}
