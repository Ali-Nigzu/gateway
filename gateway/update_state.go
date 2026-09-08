package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	updatePendingHeader        = "camos-gateway-update-pending-v1"
	updatePendingMaximumBytes  = 1024
	lifecycleStatusHeader      = "camos-gateway-lifecycle-status-v1"
	lifecycleStatusMaxDetail   = 240
	lifecycleOperationLockWait = 30 * time.Second
)

type updatePendingRecord struct {
	gatewayID       uuid.UUID
	fromVersion     string
	toVersion       string
	activeSHA256    [sha256.Size]byte
	candidateSHA256 [sha256.Size]byte
	target          string
}

type updateStartupAction uint8

const (
	updateStartupCleanup updateStartupAction = iota
	updateStartupRunCandidate
	updateStartupRollback
)

func acquireGatewayUpdateLocks(timeout time.Duration) (func(), error) {
	releaseDownload, err := acquireGatewayDownloadLock(timeout)
	if err != nil {
		return nil, err
	}
	releaseLifecycle, err := acquireGatewayLifecycleLock(timeout)
	if err != nil {
		releaseDownload()
		return nil, err
	}
	return func() {
		releaseLifecycle()
		releaseDownload()
	}, nil
}

func decideUpdateStartupAction(
	record *updatePendingRecord,
	gatewayID uuid.UUID,
	runningVersion string,
	target string,
	activeHash *[sha256.Size]byte,
	previousHash *[sha256.Size]byte,
) (updateStartupAction, error) {
	if record == nil {
		return updateStartupCleanup, nil
	}
	if gatewayID == uuid.Nil || record.gatewayID != gatewayID {
		return updateStartupCleanup, errors.New("pending update belongs to another GatewayID")
	}
	if record.target != target {
		return updateStartupCleanup, errors.New("pending update target does not match this Gateway")
	}
	if activeHash != nil && *activeHash == record.activeSHA256 {
		if runningVersion != record.fromVersion {
			return updateStartupCleanup, errors.New("pending update source version is inconsistent")
		}
		return updateStartupCleanup, nil
	}
	if activeHash != nil && *activeHash == record.candidateSHA256 && runningVersion == record.toVersion {
		return updateStartupRunCandidate, nil
	}
	if previousHash != nil && *previousHash == record.activeSHA256 {
		return updateStartupRollback, nil
	}
	return updateStartupCleanup, errors.New("last-known-good Gateway executable is unavailable")
}

func previousExecutablePath() (string, error) {
	active, err := installedExecutablePath()
	if err != nil {
		return "", err
	}
	return active + ".previous", nil
}

func validateLifecycleVersion(version string) error {
	if !utf8.ValidString(version) || validateArtifactVersion(version) != nil {
		return errors.New("release version is invalid")
	}
	return nil
}

func marshalUpdatePending(record updatePendingRecord) ([]byte, error) {
	if record.gatewayID == uuid.Nil ||
		validateLifecycleVersion(record.fromVersion) != nil ||
		validateLifecycleVersion(record.toVersion) != nil ||
		record.fromVersion == record.toVersion {
		return nil, errors.New("pending update record is invalid")
	}
	expectedTarget, err := artifactPlatformFilenameForTarget(record.target)
	if err != nil || expectedTarget != record.target {
		return nil, errors.New("pending update target is invalid")
	}
	encoded := fmt.Sprintf(
		"%s\ngateway_id=%s\nfrom_version=%s\nto_version=%s\nactive_sha256=%s\ncandidate_sha256=%s\ntarget=%s\n",
		updatePendingHeader,
		record.gatewayID.String(),
		record.fromVersion,
		record.toVersion,
		hex.EncodeToString(record.activeSHA256[:]),
		hex.EncodeToString(record.candidateSHA256[:]),
		record.target,
	)
	if len(encoded) > updatePendingMaximumBytes {
		return nil, errors.New("pending update record is too large")
	}
	return []byte(encoded), nil
}

func parseUpdatePending(encoded []byte) (updatePendingRecord, error) {
	var record updatePendingRecord
	if len(encoded) == 0 || len(encoded) > updatePendingMaximumBytes ||
		!utf8.Valid(encoded) {
		return record, errors.New("pending update record is invalid")
	}
	lines := strings.Split(string(encoded), "\n")
	if len(lines) != 8 || lines[0] != updatePendingHeader || lines[7] != "" {
		return record, errors.New("pending update record is invalid")
	}
	values := make([]string, 6)
	keys := []string{
		"gateway_id=", "from_version=", "to_version=",
		"active_sha256=", "candidate_sha256=", "target=",
	}
	for index, key := range keys {
		if !strings.HasPrefix(lines[index+1], key) {
			return record, errors.New("pending update record is invalid")
		}
		values[index] = strings.TrimPrefix(lines[index+1], key)
		if values[index] == "" {
			return record, errors.New("pending update record is invalid")
		}
	}
	gatewayID, err := uuid.Parse(values[0])
	if err != nil || gatewayID == uuid.Nil || values[0] != gatewayID.String() {
		return record, errors.New("pending update GatewayID is invalid")
	}
	if validateLifecycleVersion(values[1]) != nil ||
		validateLifecycleVersion(values[2]) != nil || values[1] == values[2] {
		return record, errors.New("pending update version is invalid")
	}
	activeHash, err := decodeCanonicalSHA256(values[3])
	if err != nil {
		return record, errors.New("pending update active hash is invalid")
	}
	candidateHash, err := decodeCanonicalSHA256(values[4])
	if err != nil {
		return record, errors.New("pending update candidate hash is invalid")
	}
	if _, err := artifactPlatformFilenameForTarget(values[5]); err != nil {
		return record, errors.New("pending update target is invalid")
	}
	return updatePendingRecord{
		gatewayID:       gatewayID,
		fromVersion:     values[1],
		toVersion:       values[2],
		activeSHA256:    activeHash,
		candidateSHA256: candidateHash,
		target:          values[5],
	}, nil
}

// artifactPlatformFilenameForTarget validates one of the four fixed release
// filenames without deriving authority from a suffix or path normalization.
func artifactPlatformFilenameForTarget(target string) (string, error) {
	switch target {
	case "windows-amd64.exe", "darwin-arm64", "darwin-amd64", "linux-amd64":
		return target, nil
	default:
		return "", errors.New("unsupported Gateway release target")
	}
}

func decodeCanonicalSHA256(encoded string) ([sha256.Size]byte, error) {
	var result [sha256.Size]byte
	if len(encoded) != hex.EncodedLen(len(result)) || encoded != strings.ToLower(encoded) {
		return result, errors.New("SHA-256 is not canonical")
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil || len(decoded) != len(result) {
		return result, errors.New("SHA-256 is invalid")
	}
	copy(result[:], decoded)
	return result, nil
}

func loadUpdatePending(paths identityPaths) (*updatePendingRecord, error) {
	file, err := os.Open(paths.updatePending)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("pending update record is unavailable")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 ||
		info.Size() > updatePendingMaximumBytes {
		return nil, errors.New("pending update record is invalid")
	}
	encoded, err := io.ReadAll(io.LimitReader(file, updatePendingMaximumBytes+1))
	if err != nil || len(encoded) > updatePendingMaximumBytes {
		return nil, errors.New("pending update record is unavailable")
	}
	record, err := parseUpdatePending(encoded)
	if err != nil {
		return nil, err
	}
	// Never use a merely visible transition record as replacement authority.
	// A successful directory sync closes a prior publisher's ambiguous fsync
	// result; SameFile then proves the pathname still names the file we read.
	if err := requireDurableIdentityEntry(paths.directory, paths.updatePending, info); err != nil {
		return nil, errors.New("pending update record durability is unavailable")
	}
	return &record, nil
}

func saveUpdatePending(paths identityPaths, record updatePendingRecord) error {
	encoded, err := marshalUpdatePending(record)
	if err != nil {
		return err
	}
	// A pending transition is a create-only commit record. Never replace an
	// existing record, even if a second process raced the preceding inspection.
	if err := atomicWriteIdentityFile(paths, paths.updatePending, encoded, false); err != nil {
		// Only the explicit post-publication directory-sync ambiguity may adopt an
		// exact visible record. A collision or any pre-publication failure remains
		// a losing concurrent publication, never success.
		if errors.Is(err, errIdentityPublishedDurabilityUnknown) {
			published, loadErr := loadUpdatePending(paths)
			if loadErr == nil && published != nil && *published == record {
				return nil
			}
		}
		return errors.New("pending update record persistence failed")
	}
	return nil
}

func removeUpdatePending(paths identityPaths) error {
	if err := os.Remove(paths.updatePending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("pending update record removal failed")
	}
	return syncIdentityDirectory(paths.directory)
}

func hashLifecycleFile(path string) ([sha256.Size]byte, int64, error) {
	var empty [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return empty, 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return empty, 0, errors.New("lifecycle file is not a non-empty regular file")
	}
	hash := sha256.New()
	written, err := io.Copy(hash, file)
	if err != nil || written != info.Size() {
		return empty, 0, errors.New("lifecycle file hash failed")
	}
	var result [sha256.Size]byte
	copy(result[:], hash.Sum(nil))
	return result, written, nil
}

func preparePreviousExecutable(activePath, previousPath string) ([sha256.Size]byte, error) {
	var empty [sha256.Size]byte
	if previousPath != activePath+".previous" ||
		filepath.Clean(filepath.Dir(previousPath)) != filepath.Clean(filepath.Dir(activePath)) {
		return empty, errors.New("previous Gateway executable path is invalid")
	}
	temporaryPath := previousPath + ".preparing"
	source, err := os.Open(activePath)
	if err != nil {
		return empty, errors.New("active Gateway executable is unavailable")
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return empty, errors.New("active Gateway executable is invalid")
	}
	temporary, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		return empty, errors.New("previous Gateway executable creation failed")
	}
	keepTemporary := true
	defer func() {
		_ = temporary.Close()
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(temporary, hash), source)
	if err != nil || written != info.Size() {
		return empty, errors.New("previous Gateway executable copy failed")
	}
	if err := temporary.Sync(); err != nil {
		return empty, errors.New("previous Gateway executable sync failed")
	}
	if err := temporary.Close(); err != nil {
		return empty, errors.New("previous Gateway executable close failed")
	}
	if err := secureInstalledExecutable(temporaryPath); err != nil {
		return empty, errors.New("previous Gateway executable permissions failed")
	}
	var copiedHash [sha256.Size]byte
	copy(copiedHash[:], hash.Sum(nil))
	activeHash, activeSize, err := hashLifecycleFile(activePath)
	if err != nil || activeSize != written || activeHash != copiedHash {
		return empty, errors.New("active Gateway executable changed during preparation")
	}
	verifiedHash, verifiedSize, err := hashLifecycleFile(temporaryPath)
	if err != nil || verifiedSize != written || verifiedHash != copiedHash {
		return empty, errors.New("previous Gateway executable verification failed")
	}
	// A matching published previous file may belong to another process that is
	// about to publish update.pending. Reuse it; never delete shared recovery
	// state merely because this process arrived second.
	if publishedHash, publishedSize, publishedErr := hashLifecycleFile(previousPath); publishedErr == nil {
		if publishedSize != written || publishedHash != copiedHash {
			return empty, errors.New("another Gateway update preparation is in progress")
		}
		if err := secureInstalledExecutable(previousPath); err != nil {
			return empty, errors.New("previous Gateway executable permissions failed")
		}
		// Another process may have observed a post-rename directory-sync failure.
		// Make the shared recovery entry durable before it can support a pending
		// commit and active replacement in this process.
		if err := syncParentDirectory(previousPath); err != nil {
			return empty, errors.New("previous Gateway executable directory sync failed")
		}
		return copiedHash, nil
	} else if !errors.Is(publishedErr, os.ErrNotExist) {
		return empty, errors.New("previous Gateway executable is unavailable")
	}
	if err := replaceFileAtomically(temporaryPath, previousPath); err != nil {
		return empty, errors.New("previous Gateway executable publication failed")
	}
	keepTemporary = false
	if err := syncParentDirectory(previousPath); err != nil {
		return empty, errors.New("previous Gateway executable directory sync failed")
	}
	publishedHash, publishedSize, err := hashLifecycleFile(previousPath)
	if err != nil || publishedSize != written || publishedHash != copiedHash {
		return empty, errors.New("published previous Gateway executable is invalid")
	}
	return copiedHash, nil
}

func prepareUpdateCommit(
	candidatePath string,
	fromVersion string,
	toVersion string,
	target string,
	gatewayID uuid.UUID,
) (updatePendingRecord, error) {
	release, err := acquireGatewayUpdateLocks(lifecycleOperationLockWait)
	if err != nil {
		return updatePendingRecord{}, err
	}
	defer release()
	return prepareUpdateCommitLocked(candidatePath, fromVersion, toVersion, target, gatewayID)
}

// prepareUpdateCommitLocked requires the platform lifecycle lock to remain
// held through final revalidation and replacement. The exported package-local
// wrapper above keeps direct callers serialized as well.
func prepareUpdateCommitLocked(
	candidatePath string,
	fromVersion string,
	toVersion string,
	target string,
	gatewayID uuid.UUID,
) (updatePendingRecord, error) {
	var empty updatePendingRecord
	if gatewayID == uuid.Nil || fromVersion != BuildVersion || fromVersion == toVersion ||
		validateLifecycleVersion(fromVersion) != nil || validateLifecycleVersion(toVersion) != nil {
		return empty, errors.New("Gateway update transition is invalid")
	}
	if _, err := artifactPlatformFilenameForTarget(target); err != nil {
		return empty, err
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		return empty, err
	}
	existing, err := loadUpdatePending(paths)
	if err != nil {
		return empty, err
	}
	if existing != nil {
		return empty, errors.New("another Gateway update is pending")
	}
	activePath, err := installedExecutablePath()
	if err != nil {
		return empty, err
	}
	if _, err := inspectGatewayCandidate(activePath, fromVersion, target); err != nil {
		return empty, errors.New("active Gateway executable identity is invalid")
	}
	previousPath, err := previousExecutablePath()
	if err != nil {
		return empty, err
	}
	candidateHash, _, err := hashLifecycleFile(candidatePath)
	if err != nil {
		return empty, errors.New("Gateway candidate hash failed")
	}
	activeHash, err := preparePreviousExecutable(activePath, previousPath)
	if err != nil {
		return empty, err
	}
	// Close the check/use window around previous publication. If another
	// process committed first, all candidate/previous paths are shared and
	// therefore belong to that transition until startup reconciliation.
	existing, err = loadUpdatePending(paths)
	if err != nil {
		return empty, err
	}
	if existing != nil {
		return empty, errors.New("another Gateway update is pending")
	}
	record := updatePendingRecord{
		gatewayID:       gatewayID,
		fromVersion:     fromVersion,
		toVersion:       toVersion,
		activeSHA256:    activeHash,
		candidateSHA256: candidateHash,
		target:          target,
	}
	if err := saveUpdatePending(paths, record); err != nil {
		return empty, err
	}
	return record, nil
}

func cancelPreparedUpdate(candidatePath string) error {
	release, err := acquireGatewayUpdateLocks(lifecycleOperationLockWait)
	if err != nil {
		return err
	}
	defer release()
	return cancelPreparedUpdateLocked(candidatePath)
}

func cancelPreparedUpdateLocked(candidatePath string) error {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	previousPath, err := previousExecutablePath()
	if err != nil {
		return err
	}
	// Keep update.pending as visible exclusion/recovery authority until every
	// shared executable path is cleaned and synchronized. Removing the record
	// first would let another process prepare and then lose its only previous
	// image to this cleanup attempt.
	for _, path := range []string{candidatePath, candidatePath + ".downloading", previousPath + ".preparing", previousPath} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("prepared Gateway update cleanup failed")
		}
	}
	if err := syncParentDirectory(previousPath); err != nil {
		return errors.New("prepared Gateway update cleanup sync failed")
	}
	return removeUpdatePending(paths)
}

func runtimeReleaseTarget() (string, error) {
	target, err := artifactPlatformFilename(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	return artifactPlatformFilenameForTarget(target)
}

// reconcileUpdateStateAtStartup derives the only safe action from the active,
// previous and pending files. It never treats ambiguous state as permission to
// run camera work. A true result means native supervision must restart after a
// locally committed rollback.
func reconcileUpdateStateAtStartup(gatewayID uuid.UUID) (bool, error) {
	release, err := acquireGatewayUpdateLocks(lifecycleOperationLockWait)
	if err != nil {
		return false, err
	}
	defer release()
	return reconcileUpdateStateLocked(gatewayID)
}

func reconcileUpdateStateLocked(gatewayID uuid.UUID) (bool, error) {
	if gatewayID == uuid.Nil {
		return false, errors.New("pending update GatewayID is unavailable")
	}
	if pending, err := gatewayRemovalPending(); err != nil {
		return false, err
	} else if pending {
		return false, errTerminalRemovalCommitted
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		return false, err
	}
	candidatePath, err := candidateExecutablePath()
	if err != nil {
		return false, err
	}
	previousPath, err := previousExecutablePath()
	if err != nil {
		return false, err
	}
	record, err := loadUpdatePending(paths)
	if err != nil {
		_ = writeLifecycleStatus(lifecycleStatusRollback, BuildVersion, err.Error())
		return false, err
	}
	if record == nil {
		// Holding the lifecycle lock while observing no pending record proves no
		// legitimate preparation can own these deterministic stale paths.
		for _, stale := range []string{
			candidatePath + ".downloading", candidatePath,
			previousPath + ".preparing", previousPath,
		} {
			if err := os.Remove(stale); err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, errors.New("stale Gateway update cleanup failed")
			}
		}
		return false, nil
	}
	if record.gatewayID != gatewayID {
		return false, errors.New("pending update belongs to another GatewayID")
	}
	target, err := runtimeReleaseTarget()
	if err != nil || record.target != target {
		return false, errors.New("pending update target does not match this Gateway")
	}
	activePath, err := installedExecutablePath()
	if err != nil {
		return false, err
	}
	var previousHash *[sha256.Size]byte
	if hash, _, hashErr := hashLifecycleFile(previousPath); hashErr == nil {
		previousHash = &hash
	} else if !errors.Is(hashErr, os.ErrNotExist) {
		return false, errors.New("last-known-good Gateway executable is unavailable")
	}
	// An exact previous image is safe to restore even when the active pathname
	// became unreadable. This lets a still-running process recover before the
	// next native launch would cross the accepted no-launch repair boundary.
	var activeHash *[sha256.Size]byte
	if hash, _, hashErr := hashLifecycleFile(activePath); hashErr == nil {
		activeHash = &hash
	}
	action, err := decideUpdateStartupAction(
		record,
		gatewayID,
		BuildVersion,
		target,
		activeHash,
		previousHash,
	)
	if err != nil {
		_ = writeLifecycleStatus(lifecycleStatusRollback, record.toVersion, err.Error())
		return false, err
	}
	switch action {
	case updateStartupCleanup:
		// Replacement never crossed its commit boundary. The running previous
		// release is still authoritative, so discard the abandoned preparation.
		return false, cancelPreparedUpdateLocked(candidatePath)
	case updateStartupRunCandidate:
		if _, err := inspectGatewayCandidate(activePath, record.toVersion, record.target); err != nil {
			return rollbackPendingUpdate(record, previousPath, activePath)
		}
		return false, nil
	case updateStartupRollback:
		return rollbackPendingUpdate(record, previousPath, activePath)
	default:
		return false, errors.New("pending update recovery action is invalid")
	}
}

func rollbackPendingUpdate(
	record *updatePendingRecord,
	previousPath string,
	activePath string,
) (bool, error) {
	if record == nil {
		return false, errors.New("pending update rollback state is unavailable")
	}
	previousHash, _, err := hashLifecycleFile(previousPath)
	if err != nil || previousHash != record.activeSHA256 {
		failure := errors.New("last-known-good Gateway executable is unavailable")
		_ = writeLifecycleStatus(lifecycleStatusRollback, record.toVersion, failure.Error())
		return false, failure
	}
	if _, err := inspectGatewayCandidate(previousPath, record.fromVersion, record.target); err != nil {
		failure := errors.New("last-known-good Gateway identity is invalid")
		_ = writeLifecycleStatus(lifecycleStatusRollback, record.toVersion, failure.Error())
		return false, failure
	}
	outcome, err := applyGatewayRollback(previousPath)
	if err != nil {
		_ = writeLifecycleStatus(lifecycleStatusRollback, record.toVersion, err.Error())
	}
	if outcome == gatewayReplacementPostCommit || outcome == gatewayReplacementHelperHandoff {
		return true, err
	}
	if err == nil {
		err = errors.New("Gateway rollback did not commit")
	}
	return false, err
}

// confirmPendingUpdate runs only after runtime identity was loaded and the
// authoritative refresh synchronously wrote reported_version=BuildVersion.
func confirmPendingUpdate(gatewayID uuid.UUID) error {
	release, err := acquireGatewayUpdateLocks(lifecycleOperationLockWait)
	if err != nil {
		return err
	}
	defer release()
	return confirmPendingUpdateLocked(gatewayID)
}

func confirmPendingUpdateLocked(gatewayID uuid.UUID) error {
	if pending, err := gatewayRemovalPending(); err != nil {
		return err
	} else if pending {
		return errTerminalRemovalCommitted
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	record, err := loadUpdatePending(paths)
	if err != nil || record == nil {
		return err
	}
	if record.gatewayID != gatewayID || record.toVersion != BuildVersion {
		return errors.New("pending update confirmation identity is inconsistent")
	}
	target, err := runtimeReleaseTarget()
	if err != nil || record.target != target {
		return errors.New("pending update confirmation target is inconsistent")
	}
	activePath, err := installedExecutablePath()
	if err != nil {
		return err
	}
	activeHash, _, err := hashLifecycleFile(activePath)
	if err != nil || activeHash != record.candidateSHA256 {
		return errors.New("pending update active hash is inconsistent")
	}
	if _, err := inspectGatewayCandidate(activePath, record.toVersion, record.target); err != nil {
		return errors.New("pending update active identity is inconsistent")
	}
	previousPath, err := previousExecutablePath()
	if err != nil {
		return err
	}
	if previousHash, _, hashErr := hashLifecycleFile(previousPath); hashErr == nil {
		if previousHash != record.activeSHA256 {
			return errors.New("pending update previous hash is inconsistent")
		}
	} else if !errors.Is(hashErr, os.ErrNotExist) {
		return errors.New("pending update previous executable is unavailable")
	}
	if err := os.Remove(previousPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("pending update previous executable removal failed")
	}
	if err := syncParentDirectory(previousPath); err != nil {
		return errors.New("pending update previous executable cleanup sync failed")
	}
	if err := removeUpdatePending(paths); err != nil {
		return err
	}
	for _, stale := range []string{candidatePathOrEmpty(), previousPath + ".preparing"} {
		if stale != "" {
			_ = os.Remove(stale)
			_ = os.Remove(stale + ".downloading")
		}
	}
	_ = removeLifecycleStatus(paths)
	return nil
}

// reconcileAndConfirmPendingUpdate keeps recovery, confirmation and the final
// local-removal check in one cross-process critical section. It runs only after
// a fresh authoritative DB refresh has reported this process's BuildVersion.
func reconcileAndConfirmPendingUpdate(
	gatewayID uuid.UUID,
) (bool, bool, lifecycleStatusCategory, error) {
	release, err := acquireGatewayUpdateLocks(lifecycleOperationLockWait)
	if err != nil {
		return false, false, lifecycleStatusRollback, err
	}
	defer release()
	handoff, err := reconcileUpdateStateLocked(gatewayID)
	if errors.Is(err, errTerminalRemovalCommitted) {
		return false, true, lifecycleStatusTerminalRemoval, nil
	}
	if err != nil || handoff {
		return handoff, false, lifecycleStatusRollback, err
	}
	if err := confirmPendingUpdateLocked(gatewayID); errors.Is(err, errTerminalRemovalCommitted) {
		return false, true, lifecycleStatusTerminalRemoval, nil
	} else if err != nil {
		return false, false, lifecycleStatusConfirmation, err
	}
	pending, err := gatewayRemovalPending()
	if err != nil {
		return false, false, lifecycleStatusTerminalRemoval, err
	}
	return false, pending, lifecycleStatusTerminalRemoval, nil
}

func candidatePathOrEmpty() string {
	path, err := candidateExecutablePath()
	if err != nil {
		return ""
	}
	return path
}

type lifecycleStatusCategory string

const (
	lifecycleStatusMetadata          lifecycleStatusCategory = "metadata"
	lifecycleStatusArtifactContract  lifecycleStatusCategory = "artifact-contract"
	lifecycleStatusDownload          lifecycleStatusCategory = "download"
	lifecycleStatusStall             lifecycleStatusCategory = "stall"
	lifecycleStatusSize              lifecycleStatusCategory = "size"
	lifecycleStatusSHA               lifecycleStatusCategory = "sha"
	lifecycleStatusCandidateIdentity lifecycleStatusCategory = "candidate-identity"
	lifecycleStatusRevalidation      lifecycleStatusCategory = "revalidation"
	lifecycleStatusReplacement       lifecycleStatusCategory = "replacement"
	lifecycleStatusRestart           lifecycleStatusCategory = "restart"
	lifecycleStatusConfirmation      lifecycleStatusCategory = "confirmation"
	lifecycleStatusRollback          lifecycleStatusCategory = "rollback"
	lifecycleStatusTerminalRemoval   lifecycleStatusCategory = "terminal-removal"
)

func validLifecycleStatusCategory(category lifecycleStatusCategory) bool {
	switch category {
	case lifecycleStatusMetadata, lifecycleStatusArtifactContract,
		lifecycleStatusDownload, lifecycleStatusStall, lifecycleStatusSize,
		lifecycleStatusSHA, lifecycleStatusCandidateIdentity,
		lifecycleStatusRevalidation, lifecycleStatusReplacement,
		lifecycleStatusRestart, lifecycleStatusConfirmation,
		lifecycleStatusRollback, lifecycleStatusTerminalRemoval:
		return true
	default:
		return false
	}
}

func sanitizeLifecycleDetail(detail string) string {
	detail = strings.TrimSpace(detail)
	lower := strings.ToLower(detail)
	for _, sensitive := range []string{
		"://", "access_token", "authorization", "credential", "private key",
		"signedurl", "signed_url", "x-goog-signature",
	} {
		if strings.Contains(lower, sensitive) {
			return "redacted detail"
		}
	}
	var builder strings.Builder
	for _, character := range detail {
		if character < 0x20 || character == 0x7f || character == '=' {
			builder.WriteByte(' ')
		} else {
			builder.WriteRune(character)
		}
		if builder.Len() >= lifecycleStatusMaxDetail {
			break
		}
	}
	result := strings.Join(strings.Fields(builder.String()), " ")
	if result == "" {
		return "unspecified failure"
	}
	if len(result) > lifecycleStatusMaxDetail {
		result = result[:lifecycleStatusMaxDetail]
	}
	return result
}

func writeLifecycleStatus(
	category lifecycleStatusCategory,
	target string,
	detail string,
) error {
	return writeLifecycleStatusAt(category, target, detail, time.Now().UTC())
}

func writeLifecycleStatusAt(
	category lifecycleStatusCategory,
	target string,
	detail string,
	now time.Time,
) error {
	if !validLifecycleStatusCategory(category) || validateLifecycleVersion(target) != nil {
		return errors.New("lifecycle status is invalid")
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	encoded := fmt.Sprintf(
		"%s\ntime=%s\nbuild=%s\ncategory=%s\ntarget=%s\ndetail=%s\n",
		lifecycleStatusHeader,
		now.UTC().Format(time.RFC3339),
		BuildVersion,
		category,
		target,
		sanitizeLifecycleDetail(detail),
	)
	if len(encoded) > 512 {
		return errors.New("lifecycle status is too large")
	}
	if err := atomicWriteIdentityFile(paths, paths.lifecycleStatus, []byte(encoded), true); err != nil {
		return errors.New("lifecycle status persistence failed")
	}
	return nil
}

func removeLifecycleStatus(paths identityPaths) error {
	if err := os.Remove(paths.lifecycleStatus); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("lifecycle status removal failed")
	}
	return syncIdentityDirectory(paths.directory)
}
