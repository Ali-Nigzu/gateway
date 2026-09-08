package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

const (
	legacyRemovalMarkerContents = "terminal-removal\n"
	removalMarkerV1Prefix       = "terminal-removal:v1:"
)

type removalMarker struct {
	gatewayID uuid.UUID
	legacy    bool
}

var errTerminalRemovalCommitted = errors.New("terminal Gateway removal is committed")

// gatewayReplacementOutcome makes the active-byte commit boundary explicit.
// A Windows helper handoff is distinct from both sides: the parent has not
// committed bytes, but must exit so the acknowledged transient helper can do
// so while retaining update.pending and the last-known-good copy.
type gatewayReplacementOutcome uint8

const (
	gatewayReplacementPreCommit gatewayReplacementOutcome = iota
	gatewayReplacementPostCommit
	gatewayReplacementHelperHandoff
)

func createRemovalMarker(gatewayID uuid.UUID) ([]byte, error) {
	if gatewayID == uuid.Nil {
		return nil, errors.New("terminal removal GatewayID is invalid")
	}
	return []byte(removalMarkerV1Prefix + gatewayID.String() + "\n"), nil
}

func parseRemovalMarker(contents []byte) (removalMarker, error) {
	if string(contents) == legacyRemovalMarkerContents {
		return removalMarker{legacy: true}, nil
	}
	encoded := string(contents)
	if !strings.HasPrefix(encoded, removalMarkerV1Prefix) ||
		len(encoded) != len(removalMarkerV1Prefix)+36+1 ||
		encoded[len(encoded)-1] != '\n' {
		return removalMarker{}, errors.New("terminal removal marker is invalid")
	}
	encodedGatewayID := encoded[len(removalMarkerV1Prefix) : len(encoded)-1]
	gatewayID, err := uuid.Parse(encodedGatewayID)
	if err != nil || gatewayID == uuid.Nil || encodedGatewayID != gatewayID.String() {
		return removalMarker{}, errors.New("terminal removal marker is invalid")
	}
	return removalMarker{gatewayID: gatewayID}, nil
}

func validateRemovalMarker(marker removalMarker, currentGatewayID uuid.UUID) error {
	if marker.legacy {
		return nil
	}
	if marker.gatewayID == uuid.Nil || currentGatewayID == uuid.Nil ||
		marker.gatewayID != currentGatewayID {
		return errors.New("terminal removal marker GatewayID does not match")
	}
	return nil
}

func readRemovalMarker(path string) (removalMarker, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return removalMarker{}, false, nil
	}
	if err != nil {
		return removalMarker{}, false, errors.New("terminal removal marker is unavailable")
	}
	if !info.Mode().IsRegular() ||
		(info.Size() != int64(len(legacyRemovalMarkerContents)) &&
			info.Size() != int64(len(removalMarkerV1Prefix)+36+1)) {
		return removalMarker{}, false, errors.New("terminal removal marker is invalid")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return removalMarker{}, false, errors.New("terminal removal marker is unavailable")
	}
	marker, err := parseRemovalMarker(contents)
	if err != nil {
		return removalMarker{}, false, err
	}
	// A visible marker is irreversible authority only after its directory entry
	// is durable. Resynchronizing on every authority read also safely resolves a
	// prior writer's post-publication fsync ambiguity before deletion can start.
	if err := requireDurableIdentityEntry(filepath.Dir(path), path, info); err != nil {
		return removalMarker{}, false, errors.New("terminal removal marker durability is unavailable")
	}
	return marker, true, nil
}

func inspectGatewayRemovalMarker(paths identityPaths, gatewayID uuid.UUID) (bool, error) {
	if gatewayID == uuid.Nil {
		return false, errors.New("terminal removal GatewayID is invalid")
	}
	marker, present, err := readRemovalMarker(paths.removalPending)
	if err != nil || !present {
		return present, err
	}
	if err := validateRemovalMarker(marker, gatewayID); err != nil {
		return false, err
	}
	return true, nil
}

func commitGatewayRemovalMarker(paths identityPaths, gatewayID uuid.UUID) error {
	if present, err := inspectGatewayRemovalMarker(paths, gatewayID); err != nil {
		return err
	} else if present {
		return nil
	}
	contents, err := createRemovalMarker(gatewayID)
	if err != nil {
		return err
	}
	if err := atomicWriteIdentityFile(
		paths,
		paths.removalPending,
		contents,
		false,
	); err != nil {
		// atomicWriteIdentityFile can report a directory-sync failure after the
		// marker rename is already visible. That is past the local irreversible
		// boundary: re-read the exact Gateway-bound marker and, if it is present,
		// keep removal authority instead of allowing normal work to resume.
		present, inspectErr := inspectGatewayRemovalMarker(paths, gatewayID)
		if inspectErr == nil && present {
			return nil
		}
		return errors.New("terminal removal marker persistence failed")
	}
	return nil
}

func markGatewayRemovalPending() error {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	gatewayID, err := loadGatewayID()
	if err != nil {
		return errors.New("terminal removal GatewayID is unavailable")
	}
	return commitGatewayRemovalMarker(paths, gatewayID)
}

func gatewayRemovalPending() (bool, error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return false, err
	}
	marker, present, err := readRemovalMarker(paths.removalPending)
	if err != nil {
		return false, err
	}
	if !present || marker.legacy {
		return present, nil
	}
	identityPresent, err := gatewayIDCommitPresent()
	if err != nil {
		return false, errors.New("terminal removal GatewayID is unavailable")
	}
	if !identityPresent {
		// A committed v1 marker with no remaining local identity can only move
		// toward absence. Commissioning independently refuses every marker, so
		// accepting this partial-removal state cannot delete a new identity.
		return true, nil
	}
	currentGatewayID, err := loadGatewayID()
	if err != nil {
		return false, errors.New("terminal removal GatewayID is unavailable")
	}
	if err := validateRemovalMarker(marker, currentGatewayID); err != nil {
		return false, err
	}
	return true, nil
}

// posixRemovalPending prevents a surviving native removal unit from being
// mistaken for normal startup authority if its marker is missing or corrupt.
// The native helper still requires the marker and therefore cannot delete from
// this ambiguous state; the permanent service must remain fail-closed too.
func posixRemovalPending() (bool, error) {
	pending, err := gatewayRemovalPending()
	if err != nil || pending {
		return pending, err
	}
	nativePending, err := platformRemovalStatePresent()
	if err != nil {
		return false, err
	}
	return decidePOSIXRemovalStartup(pending, nativePending)
}

func decidePOSIXRemovalStartup(markerPending, nativePending bool) (bool, error) {
	if markerPending {
		return true, nil
	}
	if nativePending {
		return false, errors.New("terminal removal native state exists without a valid marker")
	}
	return false, nil
}

func readRemovalGatewayID(path string) (uuid.UUID, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return uuid.Nil, errors.New("terminal removal GatewayID is unavailable")
	}
	gatewayID, err := uuid.Parse(string(encoded))
	if err != nil || gatewayID == uuid.Nil || string(encoded) != gatewayID.String() {
		return uuid.Nil, errors.New("terminal removal GatewayID is invalid")
	}
	return gatewayID, nil
}

// prepareIdentityForFinalRemoval erases durable credentials while retaining
// only the non-secret GatewayID, authorization marker, and currently running
// transient helper. Retaining GatewayID allows every retry to independently
// validate a v1 marker through sensitive cleanup. The platform helper then
// removes GatewayID and itself as its explicit completion boundary; the native
// wrapper retains the marker until the final identity-directory removal.
func prepareIdentityForFinalRemoval(paths identityPaths, helperPath string) error {
	directory := filepath.Clean(paths.directory)
	workDirectory := filepath.Clean(paths.workDirectory)
	markerPath := filepath.Clean(paths.removalPending)
	gatewayIDPath := filepath.Clean(paths.gatewayID)
	helperPath = filepath.Clean(helperPath)
	if directory == "." || directory == string(os.PathSeparator) ||
		filepath.Clean(filepath.Dir(workDirectory)) != directory ||
		filepath.Clean(filepath.Dir(markerPath)) != directory ||
		paths.gatewayID == "" || filepath.Clean(filepath.Dir(gatewayIDPath)) != directory ||
		filepath.Clean(filepath.Dir(helperPath)) != workDirectory {
		return errors.New("terminal removal paths are invalid")
	}
	helperInfo, err := os.Lstat(helperPath)
	if err != nil || !helperInfo.Mode().IsRegular() {
		return errors.New("terminal removal helper is invalid")
	}
	marker, present, err := readRemovalMarker(markerPath)
	if err != nil || !present {
		return errors.New("terminal removal marker is invalid")
	}
	if !marker.legacy {
		_, err := os.Lstat(gatewayIDPath)
		identityPresent := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("terminal removal GatewayID is unavailable")
		}
		if identityPresent {
			gatewayID, err := readRemovalGatewayID(gatewayIDPath)
			if err != nil || validateRemovalMarker(marker, gatewayID) != nil {
				return errors.New("terminal removal marker is invalid")
			}
		}
	}

	entries, err := os.ReadDir(directory)
	if err != nil {
		return errors.New("Gateway identity inspection failed")
	}
	for _, entry := range entries {
		path := filepath.Join(directory, entry.Name())
		if filepath.Clean(path) == markerPath || filepath.Clean(path) == gatewayIDPath ||
			filepath.Clean(path) == workDirectory {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return errors.New("Gateway identity preparation failed")
		}
	}

	workEntries, err := os.ReadDir(workDirectory)
	if err != nil {
		return errors.New("Gateway lifecycle state inspection failed")
	}
	for _, entry := range workEntries {
		path := filepath.Join(workDirectory, entry.Name())
		if filepath.Clean(path) == helperPath {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return errors.New("Gateway lifecycle state preparation failed")
		}
	}
	if err := syncIdentityDirectory(workDirectory); err != nil {
		return err
	}
	return syncIdentityDirectory(directory)
}
