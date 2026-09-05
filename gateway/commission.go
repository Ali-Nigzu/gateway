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
	paths      identityPaths
	hash       commissionHash
	privateKey *rsa.PrivateKey
	committed  bool
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
		if err != nil {
			var httpError *commissionHTTPError
			if errors.As(err, &httpError) && httpError.definitiveInvalidID {
				_ = removeUncommittedIdentity(prepared.paths)
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
		if err := saveGatewayCertificate(prepared.paths, certificatePEM); err != nil {
			return errors.New("Gateway certificate persistence failed")
		}
		if err := ensureCertificateConfig(prepared.paths); err != nil {
			return err
		}
		// Prove the issued identity through X.509 WIF before making GatewayID
		// the final commit marker. A bad or unusable certificate therefore
		// remains a resumable commissioning attempt instead of permanently
		// committing an appliance that cannot authenticate.
		if err := proveRuntimeIdentity(ctx, gatewayID, prepared.paths.certificateConfig); err != nil {
			return err
		}
		// GatewayID is deliberately the final durable identity commit marker.
		if err := saveGatewayID(gatewayID); err != nil {
			return err
		}
	}

	if err := installService(); err != nil {
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
	fmt.Fprintln(os.Stdout, "COMMISSIONING COMPLETE")
	return nil
}

func prepareCommission(rawCommissionID string, now time.Time) (preparedCommission, error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return preparedCommission{}, err
	}
	if err := prepareIdentityDirectory(paths); err != nil {
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
		return preparedCommission{
			paths:      paths,
			hash:       expectedHash,
			privateKey: identity.privateKey,
			committed:  true,
		}, nil
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
	return preparedCommission{
		paths:      paths,
		hash:       expectedHash,
		privateKey: privateKey,
	}, nil
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
