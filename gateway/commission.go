package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	commissionServiceURL       = "https://gateway-commission-907308824075.europe-west2.run.app"
	commissionResponseMaxBytes = 1024 * 1024
	commissionCompletionPoll   = time.Second
)

type commissionRequest struct {
	CommissionID string `json:"commission_id"`
	PublicKeyPEM string `json:"public_key_pem"`
}

type commissionResponse struct {
	GatewayID      string `json:"gateway_id"`
	CertificatePEM string `json:"certificate_pem"`
}

type commissionHTTPError struct {
	definitiveInvalidID bool
}

func (err *commissionHTTPError) Error() string {
	if err.definitiveInvalidID {
		return "Commission ID was rejected"
	}
	return "Commission Service unavailable"
}

type preparedCommission struct {
	paths               identityPaths
	hash                commissionHash
	privateKey          *rsa.PrivateKey
	directoryGeneration os.FileInfo
	gatewayID           uuid.UUID
	committed           bool
}

func commission(rawCommissionID string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if err := requireCommissionPrivileges(); err != nil {
		return err
	}
	if rawCommissionID == "" || strings.TrimSpace(rawCommissionID) == "" {
		return errors.New("Commission ID must not be empty")
	}
	if err := validateEmbeddedRelease(); err != nil {
		return err
	}

	prepared, err := prepareCommission(rawCommissionID, time.Now())
	if err != nil {
		return err
	}
	if !prepared.committed {
		bootstrapJSON, err := bootstrapCredentialsJSON()
		if err != nil {
			return err
		}
		client, err := newBootstrapHTTPClient(ctx, bootstrapJSON)
		if err != nil {
			return err
		}
		publicKeyPEM, err := marshalGatewayPublicKey(prepared.privateKey)
		if err != nil {
			return err
		}
		response, err := commissionUntilAccepted(
			ctx,
			client,
			rawCommissionID,
			string(publicKeyPEM),
			retryDelay,
		)
		if removalErr := ensureCommissioningAllowed(prepared.paths); removalErr != nil {
			return removalErr
		}
		if err != nil {
			var httpError *commissionHTTPError
			if errors.As(err, &httpError) && httpError.definitiveInvalidID {
				// Only erase the attempt that made this rejected request. A
				// concurrent terminal removal or replacement commission may have
				// changed the canonical identity generation while the network call
				// was in flight.
				_ = cleanupRejectedCommission(prepared)
			}
			return err
		}
		gatewayID, err := uuid.Parse(response.GatewayID)
		if err != nil || gatewayID == uuid.Nil || response.GatewayID != gatewayID.String() {
			return errors.New("Commission Service returned an invalid GatewayID")
		}
		certificatePEM := []byte(response.CertificatePEM)
		if _, err := validateGatewayCertificate(
			certificatePEM,
			prepared.privateKey,
			gatewayID,
			time.Now(),
		); err != nil {
			return fmt.Errorf("Commission Service returned invalid identity: %w", err)
		}
		prepared.gatewayID = gatewayID
		if err := commitPreparedCommissionIdentity(ctx, prepared, certificatePEM); err != nil {
			return err
		}
	}

	if err := installService(prepared, time.Now()); err != nil {
		return err
	}
	if err := waitForCommissionCompletion(
		ctx,
		prepared.paths,
		prepared.hash,
		commissionCompletionPoll,
	); err != nil {
		return err
	}
	if err := validatePreparedCommissionCompletion(prepared, time.Now()); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "COMMISSIONING COMPLETE")
	return nil
}

func prepareCommission(rawCommissionID string, now time.Time) (preparedCommission, error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return preparedCommission{}, err
	}
	if err := ensureCommissioningAllowed(paths); err != nil {
		return preparedCommission{}, err
	}
	release, err := acquireGatewayLifecycleLock(lifecycleOperationLockWait)
	if err != nil {
		return preparedCommission{}, err
	}
	defer release()
	if err := ensureCommissioningAllowed(paths); err != nil {
		return preparedCommission{}, err
	}
	if err := createIdentityDirectory(paths); err != nil {
		return preparedCommission{}, err
	}
	expectedHash := hashCommissionID(rawCommissionID)
	committed, err := gatewayIDCommitPresent()
	if err != nil {
		return preparedCommission{}, errors.New("GatewayID inspection failed")
	}
	pending, pendingErr := loadPendingCommissionHash(paths)

	if committed {
		identity, identityErr := loadGatewayIdentity(now)
		if identityErr != nil {
			return preparedCommission{}, fmt.Errorf("committed Gateway identity is invalid: %w", identityErr)
		}
		if pendingErr != nil {
			return preparedCommission{}, pendingErr
		}
		if pending == nil {
			return preparedCommission{}, errors.New("Gateway is already commissioned")
		}
		if !pending.matches(expectedHash) {
			return preparedCommission{}, errors.New("a different commissioning attempt is incomplete")
		}
		if err := ensureCommissioningAllowed(paths); err != nil {
			return preparedCommission{}, err
		}
		prepared := preparedCommission{
			paths:      paths,
			hash:       expectedHash,
			privateKey: identity.privateKey,
			gatewayID:  identity.gatewayID,
			committed:  true,
		}
		prepared.directoryGeneration, err = captureIdentityDirectoryGeneration(paths)
		return prepared, err
	}

	if pendingErr != nil {
		if err := removeUncommittedIdentity(paths); err != nil {
			return preparedCommission{}, err
		}
		pending = nil
	}
	if pending != nil && !pending.matches(expectedHash) {
		return preparedCommission{}, errors.New("a different commissioning attempt is incomplete")
	}
	if pending == nil {
		// Without the GatewayID commit marker, unrelated files with no matching
		// hash are definitively incomplete and may be safely replaced.
		if err := removeUncommittedIdentity(paths); err != nil {
			return preparedCommission{}, err
		}
		created, err := saveNewCommissionHash(paths, expectedHash)
		if err != nil {
			return preparedCommission{}, errors.New("pending commissioning state persistence failed")
		}
		if !created {
			pending, err = loadPendingCommissionHash(paths)
			if err != nil || pending == nil || !pending.matches(expectedHash) {
				return preparedCommission{}, errors.New("a different commissioning attempt is incomplete")
			}
		}
	}

	privateKey, err := readGatewayPrivateKey(paths.privateKey)
	if err != nil {
		if err := removeUncommittedCredentialFiles(paths); err != nil {
			return preparedCommission{}, err
		}
		privateKey, err = generateGatewayPrivateKey()
		if err != nil {
			return preparedCommission{}, err
		}
		created, err := saveNewGatewayPrivateKey(paths, privateKey)
		if err != nil {
			return preparedCommission{}, errors.New("Gateway private key persistence failed")
		}
		if !created {
			privateKey, err = readGatewayPrivateKey(paths.privateKey)
			if err != nil {
				return preparedCommission{}, err
			}
		}
	}
	if err := ensureCommissioningAllowed(paths); err != nil {
		return preparedCommission{}, err
	}
	prepared := preparedCommission{
		paths:      paths,
		hash:       expectedHash,
		privateKey: privateKey,
	}
	prepared.directoryGeneration, err = captureIdentityDirectoryGeneration(paths)
	return prepared, err
}

// validatePreparedCommissionFiles proves that a network response still
// belongs to the exact local attempt and directory generation that created its
// public key. It intentionally performs no cleanup on mismatch.
func validatePreparedCommissionFiles(prepared preparedCommission) error {
	if prepared.directoryGeneration == nil || prepared.privateKey == nil {
		return errors.New("prepared Gateway identity is invalid")
	}
	currentGeneration, err := captureIdentityDirectoryGeneration(prepared.paths)
	if err != nil || !os.SameFile(prepared.directoryGeneration, currentGeneration) {
		return errors.New("Gateway identity generation changed during commissioning")
	}
	pending, err := loadPendingCommissionHash(prepared.paths)
	if err != nil || pending == nil || !pending.matches(prepared.hash) {
		return errors.New("pending commissioning state changed unexpectedly")
	}
	privateKey, err := readGatewayPrivateKey(prepared.paths.privateKey)
	if err != nil || !publicKeysEqual(&privateKey.PublicKey, &prepared.privateKey.PublicKey) {
		return errors.New("Gateway private key changed during commissioning")
	}
	return nil
}

func commitPreparedCommissionIdentity(
	ctx context.Context,
	prepared preparedCommission,
	certificatePEM []byte,
) error {
	release, err := acquireGatewayLifecycleLock(lifecycleOperationLockWait)
	if err != nil {
		return err
	}
	defer release()
	if err := ensureCommissioningAllowed(prepared.paths); err != nil {
		return err
	}
	if err := validatePreparedCommissionFiles(prepared); err != nil {
		return err
	}
	committed, err := gatewayIDCommitPresent()
	if err != nil {
		return errors.New("GatewayID inspection failed")
	}
	if committed {
		return errors.New("Gateway identity changed during commissioning")
	}
	if err := saveGatewayCertificate(prepared.paths, certificatePEM); err != nil {
		return errors.New("Gateway certificate persistence failed")
	}
	if err := ensureCertificateConfig(prepared.paths); err != nil {
		return err
	}
	// Prove the issued identity through X.509 WIF before making GatewayID the
	// final commit marker. Holding the short lifecycle boundary across proof
	// prevents removal or another commissioning process from replacing the
	// directory between proof and publication.
	if err := proveRuntimeIdentity(ctx, prepared.gatewayID, prepared.paths.certificateConfig); err != nil {
		return err
	}
	if err := ensureCommissioningAllowed(prepared.paths); err != nil {
		return err
	}
	if err := validatePreparedCommissionFiles(prepared); err != nil {
		return err
	}
	if committed, err := gatewayIDCommitPresent(); err != nil || committed {
		return errors.New("Gateway identity changed during commissioning")
	}
	// GatewayID is deliberately the final durable identity commit marker.
	return saveGatewayID(prepared.gatewayID)
}

// validatePreparedCommissionServiceInstall runs while the platform installer
// owns the lifecycle lock. This closes the gap between the final commissioning
// check and native service/package publication without nesting platform locks.
func validatePreparedCommissionServiceInstall(prepared preparedCommission, now time.Time) error {
	currentGeneration, err := captureIdentityDirectoryGeneration(prepared.paths)
	if err != nil || prepared.directoryGeneration == nil ||
		!os.SameFile(prepared.directoryGeneration, currentGeneration) {
		return errors.New("Gateway identity generation changed during commissioning")
	}
	identity, err := loadGatewayIdentity(now)
	if err != nil || identity.gatewayID != prepared.gatewayID ||
		!publicKeysEqual(&identity.privateKey.PublicKey, &prepared.privateKey.PublicKey) {
		return errors.New("Gateway identity changed during commissioning")
	}
	if pending, pendingErr := loadPendingCommissionHash(prepared.paths); pendingErr != nil {
		return pendingErr
	} else if pending != nil && !pending.matches(prepared.hash) {
		return errors.New("pending commissioning state changed unexpectedly")
	}
	return nil
}

func validatePreparedCommissionCompletion(prepared preparedCommission, now time.Time) error {
	release, err := acquireGatewayLifecycleLock(lifecycleOperationLockWait)
	if err != nil {
		return err
	}
	defer release()
	if err := ensureCommissioningAllowed(prepared.paths); err != nil {
		return err
	}
	if err := validatePreparedCommissionServiceInstall(prepared, now); err != nil {
		return err
	}
	pending, err := loadPendingCommissionHash(prepared.paths)
	if err != nil || pending != nil {
		return errors.New("pending commissioning completion changed unexpectedly")
	}
	return nil
}

func cleanupRejectedCommission(prepared preparedCommission) error {
	release, err := acquireGatewayLifecycleLock(lifecycleOperationLockWait)
	if err != nil {
		return err
	}
	defer release()
	if err := ensureCommissioningAllowed(prepared.paths); err != nil {
		return err
	}
	if err := validatePreparedCommissionFiles(prepared); err != nil {
		return err
	}
	committed, err := gatewayIDCommitPresent()
	if err != nil || committed {
		return errors.New("Gateway identity changed during commissioning")
	}
	return removeUncommittedIdentity(prepared.paths)
}

func ensureCommissioningAllowed(paths identityPaths) error {
	nativePending, nativeErr := platformRemovalStatePresent()
	if nativeErr != nil {
		return nativeErr
	}
	if nativePending {
		return errors.New("Gateway terminal removal is pending")
	}
	_, err := os.Lstat(paths.removalPending)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("terminal removal state is unavailable")
	}
	return errors.New("Gateway terminal removal is pending")
}

func removeUncommittedCredentialFiles(paths identityPaths) error {
	for _, path := range []string{
		paths.privateKey,
		paths.certificate,
		paths.certificateConfig,
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("incomplete Gateway identity cleanup failed")
		}
	}
	return syncIdentityDirectory(paths.directory)
}

func commissionUntilAccepted(
	ctx context.Context,
	client *http.Client,
	rawCommissionID string,
	publicKeyPEM string,
	delay time.Duration,
) (commissionResponse, error) {
	return commissionUntilAcceptedAt(
		ctx,
		client,
		commissionServiceURL+"/commission",
		rawCommissionID,
		publicKeyPEM,
		delay,
	)
}

func commissionUntilAcceptedAt(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	rawCommissionID string,
	publicKeyPEM string,
	delay time.Duration,
) (commissionResponse, error) {
	for {
		response, err := requestCommission(
			ctx,
			client,
			endpoint,
			rawCommissionID,
			publicKeyPEM,
		)
		if err == nil {
			return response, nil
		}
		var httpError *commissionHTTPError
		if errors.As(err, &httpError) && httpError.definitiveInvalidID {
			return commissionResponse{}, err
		}
		if ctx.Err() != nil || !waitContext(ctx, delay) {
			return commissionResponse{}, ctx.Err()
		}
	}
}

func requestCommission(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	rawCommissionID string,
	publicKeyPEM string,
) (commissionResponse, error) {
	requestBody, err := json.Marshal(commissionRequest{
		CommissionID: rawCommissionID,
		PublicKeyPEM: publicKeyPEM,
	})
	if err != nil {
		return commissionResponse{}, errors.New("Commission Service request failed")
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint,
		bytes.NewReader(requestBody),
	)
	if err != nil {
		return commissionResponse{}, errors.New("Commission Service request failed")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return commissionResponse{}, &commissionHTTPError{}
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return commissionResponse{}, &commissionHTTPError{
			definitiveInvalidID: definitiveCommissionRejection(response.StatusCode),
		}
	}
	limited := io.LimitReader(response.Body, commissionResponseMaxBytes+1)
	encoded, err := io.ReadAll(limited)
	if err != nil || len(encoded) > commissionResponseMaxBytes {
		return commissionResponse{}, errors.New("Commission Service response is invalid")
	}
	var result commissionResponse
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return commissionResponse{}, errors.New("Commission Service response is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return commissionResponse{}, errors.New("Commission Service response is invalid")
	}
	if result.GatewayID == "" || result.CertificatePEM == "" {
		return commissionResponse{}, errors.New("Commission Service response is invalid")
	}
	return result, nil
}

func definitiveCommissionRejection(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusGone,
		http.StatusUnprocessableEntity:
		return true
	default:
		return false
	}
}

func waitForCommissionCompletion(
	ctx context.Context,
	paths identityPaths,
	expected commissionHash,
	pollInterval time.Duration,
) error {
	for {
		if err := ensureCommissioningAllowed(paths); err != nil {
			return err
		}
		encoded, err := os.ReadFile(paths.commissionHash)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err == nil {
			actual, parseErr := parseCommissionHash(encoded)
			if parseErr != nil || !actual.matches(expected) {
				return errors.New("pending commissioning state changed unexpectedly")
			}
		}
		if ctx.Err() != nil || !waitContext(ctx, pollInterval) {
			return ctx.Err()
		}
	}
}
