package main

import (
	"bytes"
	"crypto/sha256"
	gobuildinfo "debug/buildinfo"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
)

type gatewayCandidateIdentity struct {
	Version string
	Target  string
	GOOS    string
	GOARCH  string
	Module  string
	Size    int64
	SHA256  [sha256.Size]byte
}

type artifactTarget struct {
	Filename string
	GOOS     string
	GOARCH   string
}

func artifactTargetForFilename(filename string) (artifactTarget, error) {
	switch filename {
	case "windows-amd64.exe":
		return artifactTarget{Filename: filename, GOOS: "windows", GOARCH: "amd64"}, nil
	case "darwin-arm64":
		return artifactTarget{Filename: filename, GOOS: "darwin", GOARCH: "arm64"}, nil
	case "darwin-amd64":
		return artifactTarget{Filename: filename, GOOS: "darwin", GOARCH: "amd64"}, nil
	case "linux-amd64":
		return artifactTarget{Filename: filename, GOOS: "linux", GOARCH: "amd64"}, nil
	default:
		return artifactTarget{}, fmt.Errorf("unsupported Gateway artifact target %q", filename)
	}
}

// inspectGatewayCandidate verifies a downloaded executable without executing
// it. The returned digest is over the exact bytes that were inspected.
func inspectGatewayCandidate(path, requestedVersion, targetFilename string) (gatewayCandidateIdentity, error) {
	target, err := artifactTargetForFilename(targetFilename)
	if err != nil {
		return gatewayCandidateIdentity{}, err
	}
	if err := validateArtifactVersion(requestedVersion); err != nil {
		return gatewayCandidateIdentity{}, errors.New("candidate version is invalid")
	}

	info, err := os.Lstat(path)
	if err != nil {
		return gatewayCandidateIdentity{}, fmt.Errorf("candidate stat failed: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumGatewayArtifactSize {
		return gatewayCandidateIdentity{}, errors.New("candidate is not a bounded regular file")
	}
	if err := validateCandidateExecutableFamily(path, target); err != nil {
		return gatewayCandidateIdentity{}, err
	}

	goInfo, err := gobuildinfo.ReadFile(path)
	if err != nil {
		return gatewayCandidateIdentity{}, errors.New("candidate Go build information is missing or invalid")
	}
	if goInfo.Path != gatewayGoModule || goInfo.Main.Path != gatewayGoModule {
		return gatewayCandidateIdentity{}, errors.New("candidate Go main module is invalid")
	}
	goos, err := uniqueBuildSetting(goInfo, "GOOS")
	if err != nil || goos != target.GOOS {
		return gatewayCandidateIdentity{}, errors.New("candidate GOOS does not match its target")
	}
	goarch, err := uniqueBuildSetting(goInfo, "GOARCH")
	if err != nil || goarch != target.GOARCH {
		return gatewayCandidateIdentity{}, errors.New("candidate GOARCH does not match its target")
	}

	file, err := os.Open(path)
	if err != nil {
		return gatewayCandidateIdentity{}, fmt.Errorf("candidate open failed: %w", err)
	}
	hash := sha256.New()
	stamp := []byte(expectedGatewayReleaseStamp(requestedVersion, target.Filename))
	stampScanner := newBoundedStampScanner(stamp)
	written, copyErr := io.Copy(io.MultiWriter(hash, stampScanner), file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written != info.Size() {
		return gatewayCandidateIdentity{}, errors.New("candidate could not be read completely")
	}
	if stampScanner.matches != 1 {
		return gatewayCandidateIdentity{}, errors.New("candidate release stamp is missing or ambiguous")
	}

	identity := gatewayCandidateIdentity{
		Version: requestedVersion,
		Target:  target.Filename,
		GOOS:    goos,
		GOARCH:  goarch,
		Module:  goInfo.Main.Path,
		Size:    written,
	}
	copy(identity.SHA256[:], hash.Sum(nil))
	return identity, nil
}

func inspectGatewayCandidateForRuntime(path, requestedVersion string) (gatewayCandidateIdentity, error) {
	target, err := artifactPlatformFilename(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return gatewayCandidateIdentity{}, err
	}
	return inspectGatewayCandidate(path, requestedVersion, target)
}

func validateCandidateExecutableFamily(path string, target artifactTarget) error {
	switch target.GOOS {
	case "windows":
		candidate, err := pe.Open(path)
		if err != nil {
			return errors.New("candidate is not a valid PE executable")
		}
		defer candidate.Close()
		optionalHeader, optional64 := candidate.OptionalHeader.(*pe.OptionalHeader64)
		if candidate.FileHeader.Machine != pe.IMAGE_FILE_MACHINE_AMD64 || !optional64 ||
			optionalHeader.AddressOfEntryPoint == 0 ||
			candidate.FileHeader.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 ||
			candidate.FileHeader.Characteristics&pe.IMAGE_FILE_DLL != 0 {
			return errors.New("candidate PE architecture does not match its target")
		}
	case "linux":
		candidate, err := elf.Open(path)
		if err != nil {
			return errors.New("candidate is not a valid ELF executable")
		}
		defer candidate.Close()
		if candidate.FileHeader.Type != elf.ET_EXEC ||
			candidate.FileHeader.Class != elf.ELFCLASS64 ||
			candidate.FileHeader.Data != elf.ELFDATA2LSB ||
			candidate.FileHeader.Machine != elf.EM_X86_64 || candidate.FileHeader.Entry == 0 {
			return errors.New("candidate ELF architecture does not match its target")
		}
	case "darwin":
		candidate, err := macho.Open(path)
		if err != nil {
			return errors.New("candidate is not a valid Mach-O executable")
		}
		defer candidate.Close()
		expectedCPU := macho.CpuAmd64
		if target.GOARCH == "arm64" {
			expectedCPU = macho.CpuArm64
		}
		if candidate.Type != macho.TypeExec || candidate.Cpu != expectedCPU {
			return errors.New("candidate Mach-O architecture does not match its target")
		}
	default:
		return errors.New("candidate executable family is unsupported")
	}
	return nil
}

func uniqueBuildSetting(info *debug.BuildInfo, key string) (string, error) {
	value := ""
	found := false
	for _, setting := range info.Settings {
		if setting.Key != key {
			continue
		}
		if found || setting.Value == "" {
			return "", fmt.Errorf("candidate build setting %s is ambiguous", key)
		}
		value = setting.Value
		found = true
	}
	if !found {
		return "", fmt.Errorf("candidate build setting %s is missing", key)
	}
	return value, nil
}

type boundedStampScanner struct {
	wanted  []byte
	tail    []byte
	matches int
}

func newBoundedStampScanner(wanted []byte) *boundedStampScanner {
	return &boundedStampScanner{wanted: bytes.Clone(wanted)}
}

func (scanner *boundedStampScanner) Write(chunk []byte) (int, error) {
	joined := make([]byte, 0, len(scanner.tail)+len(chunk))
	joined = append(joined, scanner.tail...)
	joined = append(joined, chunk...)
	for offset := 0; ; {
		index := bytes.Index(joined[offset:], scanner.wanted)
		if index < 0 {
			break
		}
		scanner.matches++
		offset += index + len(scanner.wanted)
	}
	keep := len(scanner.wanted) - 1
	if keep > len(joined) {
		keep = len(joined)
	}
	scanner.tail = append(scanner.tail[:0], joined[len(joined)-keep:]...)
	return len(chunk), nil
}
