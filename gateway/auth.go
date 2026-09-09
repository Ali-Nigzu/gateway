package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials/externalaccount"
	"cloud.google.com/go/auth/credentials/impersonate"
	"cloud.google.com/go/auth/oauth2adapt"
	"github.com/google/uuid"
	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
	"google.golang.org/api/option"
)

const (
	bootstrapServiceAccount = "gateway-bootstrap@camosbase.iam.gserviceaccount.com"
	runtimeServiceAccount   = "gateway-runtime@camosbase.iam.gserviceaccount.com"
	runtimeDatabaseUser     = "gateway-runtime@camosbase.iam"
	runtimeProjectID        = "camosbase"
	runtimeProjectNumber    = "907308824075"
	runtimeWIFPool          = "gateway-wif"
	runtimeWIFProvider      = "camos-gateway-x509"

	runtimeWIFAudience = "//iam.googleapis.com/projects/907308824075/locations/global/" +
		"workloadIdentityPools/gateway-wif/providers/camos-gateway-x509"
	runtimeWIFTokenURL         = "https://sts.mtls.googleapis.com/v1/token"
	runtimeWIFSubjectTokenType = "urn:ietf:params:oauth:token-type:mtls"
	runtimeImpersonationURL    = "https://iamcredentials.googleapis.com/v1/projects/-/" +
		"serviceAccounts/gateway-runtime@camosbase.iam.gserviceaccount.com:generateAccessToken"

	cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
	cloudSQLAdminScope = "https://www.googleapis.com/auth/sqlservice.admin"
	cloudSQLLoginScope = "https://www.googleapis.com/auth/sqlservice.login"

	authHTTPTimeout = 90 * time.Second
)

var (
	runtimeCloudScopes = []string{cloudPlatformScope, cloudSQLAdminScope}
	runtimeLoginScopes = []string{cloudSQLLoginScope}
)

type runtimeCredentials struct {
	gatewayID                uuid.UUID
	identityGeneration       os.FileInfo
	cloudPlatformTokenSource oauth2.TokenSource
	databaseLoginTokenSource oauth2.TokenSource
	renewalTokenSource       oauth2.TokenSource
}

func newBootstrapHTTPClient(ctx context.Context, credentialsJSON []byte) (*http.Client, error) {
	if err := validateBootstrapCredentials(credentialsJSON); err != nil {
		return nil, err
	}
	baseClient := &http.Client{Timeout: authHTTPTimeout}
	authContext := context.WithValue(ctx, oauth2.HTTPClient, baseClient)
	tokenSource, err := idtoken.NewTokenSource(
		authContext,
		commissionServiceURL,
		option.WithCredentialsJSON(credentialsJSON),
	)
	if err != nil {
		return nil, errors.New("bootstrap credentials are invalid")
	}
	client := oauth2.NewClient(authContext, oauth2.ReuseTokenSource(nil, tokenSource))
	client.Timeout = authHTTPTimeout
	return client, nil
}

func validateBootstrapCredentials(encoded []byte) error {
	var metadata struct {
		Type        string `json:"type"`
		ProjectID   string `json:"project_id"`
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
		TokenURI    string `json:"token_uri"`
	}
	if err := json.Unmarshal(encoded, &metadata); err != nil {
		return errors.New("embedded bootstrap credentials are invalid")
	}
	parsedTokenURI, err := url.Parse(metadata.TokenURI)
	if err != nil || parsedTokenURI.Scheme != "https" ||
		parsedTokenURI.Host != "oauth2.googleapis.com" || parsedTokenURI.Path != "/token" ||
		parsedTokenURI.RawQuery != "" || parsedTokenURI.Fragment != "" {
		return errors.New("embedded bootstrap credentials are invalid")
	}
	if metadata.Type != "service_account" || metadata.ProjectID != runtimeProjectID ||
		metadata.ClientEmail != bootstrapServiceAccount || metadata.PrivateKey == "" {
		return errors.New("embedded bootstrap credentials are invalid")
	}
	return nil
}

func newRuntimeCredentials(ctx context.Context) (*runtimeCredentials, error) {
	release, err := acquireGatewayLifecycleLock(lifecycleOperationLockWait)
	if err != nil {
		return nil, err
	}
	defer release()
	paths, err := resolveIdentityPaths()
	if err != nil {
		return nil, err
	}
	if err := ensureCommissioningAllowed(paths); err != nil {
		return nil, err
	}
	identity, err := loadGatewayIdentity(time.Now())
	if err != nil {
		return nil, err
	}
	generation, err := captureIdentityDirectoryGeneration(paths)
	if err != nil {
		return nil, err
	}
	if err := ensureCertificateConfig(paths); err != nil {
		return nil, err
	}
	credentials, err := newRuntimeCredentialsFromConfig(
		ctx,
		identity.gatewayID,
		paths.certificateConfig,
		&http.Client{Timeout: authHTTPTimeout},
	)
	if err != nil {
		return nil, err
	}
	currentGeneration, err := captureIdentityDirectoryGeneration(paths)
	if err != nil || !os.SameFile(generation, currentGeneration) {
		return nil, errors.New("runtime identity generation changed unexpectedly")
	}
	credentials.identityGeneration = generation
	return credentials, nil
}

func validateRuntimeIdentityGeneration(credentials *runtimeCredentials) error {
	if credentials == nil || credentials.gatewayID == uuid.Nil ||
		credentials.identityGeneration == nil {
		return errors.New("runtime identity generation is unavailable")
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	return validateBoundIdentityGeneration(
		credentials.identityGeneration,
		credentials.gatewayID,
		paths,
		loadGatewayID,
	)
}

func validateBoundIdentityGeneration(
	expectedGeneration os.FileInfo,
	expectedGatewayID uuid.UUID,
	paths identityPaths,
	readGatewayID func() (uuid.UUID, error),
) error {
	if expectedGeneration == nil || expectedGatewayID == uuid.Nil || readGatewayID == nil {
		return errors.New("runtime identity generation is unavailable")
	}
	currentGeneration, err := captureIdentityDirectoryGeneration(paths)
	if err != nil || !os.SameFile(expectedGeneration, currentGeneration) {
		return errors.New("runtime identity generation changed unexpectedly")
	}
	gatewayID, err := readGatewayID()
	if err != nil || gatewayID != expectedGatewayID {
		return errors.New("runtime GatewayID changed unexpectedly")
	}
	return nil
}

func newRuntimeCredentialsFromConfig(
	ctx context.Context,
	gatewayID uuid.UUID,
	certificateConfigPath string,
	client *http.Client,
) (*runtimeCredentials, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if gatewayID == uuid.Nil || !filepath.IsAbs(certificateConfigPath) {
		return nil, errors.New("runtime identity configuration is invalid")
	}
	if client == nil {
		client = &http.Client{Timeout: authHTTPTimeout}
	}
	cloudCredentials, err := newWIFCredentials(
		certificateConfigPath,
		runtimeCloudScopes,
		true,
		client,
	)
	if err != nil {
		return nil, errors.New("runtime credentials are invalid")
	}
	loginCredentials, err := newWIFCredentials(
		certificateConfigPath,
		runtimeLoginScopes,
		true,
		client,
	)
	if err != nil {
		return nil, errors.New("runtime credentials are invalid")
	}
	directCredentials, err := newWIFCredentials(
		certificateConfigPath,
		[]string{cloudPlatformScope},
		false,
		client,
	)
	if err != nil {
		return nil, errors.New("runtime credentials are invalid")
	}
	renewalCredentials, err := impersonate.NewIDTokenCredentials(&impersonate.IDTokenOptions{
		Audience:        commissionServiceURL,
		TargetPrincipal: runtimeServiceAccount,
		IncludeEmail:    true,
		Credentials:     directCredentials,
	})
	if err != nil {
		return nil, errors.New("runtime renewal credentials are invalid")
	}
	return &runtimeCredentials{
		gatewayID: gatewayID,
		cloudPlatformTokenSource: reusableOAuth2TokenSource(
			cloudCredentials,
		),
		databaseLoginTokenSource: reusableOAuth2TokenSource(
			loginCredentials,
		),
		renewalTokenSource: reusableOAuth2TokenSource(renewalCredentials),
	}, nil
}

func runtimeWIFOptions(
	certificateConfigPath string,
	scopes []string,
	withServiceAccountImpersonation bool,
	client *http.Client,
) *externalaccount.Options {
	options := &externalaccount.Options{
		Audience:         runtimeWIFAudience,
		SubjectTokenType: runtimeWIFSubjectTokenType,
		TokenURL:         runtimeWIFTokenURL,
		CredentialSource: &externalaccount.CredentialSource{
			Certificate: &externalaccount.CertificateConfig{
				CertificateConfigLocation: certificateConfigPath,
			},
		},
		Scopes: append([]string(nil), scopes...),
		Client: client,
	}
	if withServiceAccountImpersonation {
		options.ServiceAccountImpersonationURL = runtimeImpersonationURL
		options.ServiceAccountImpersonationLifetimeSeconds = 3600
	}
	return options
}

func newWIFCredentials(
	certificateConfigPath string,
	scopes []string,
	withServiceAccountImpersonation bool,
	client *http.Client,
) (*auth.Credentials, error) {
	return externalaccount.NewCredentials(runtimeWIFOptions(
		certificateConfigPath,
		scopes,
		withServiceAccountImpersonation,
		client,
	))
}

func proveRuntimeIdentity(
	ctx context.Context,
	gatewayID uuid.UUID,
	certificateConfigPath string,
) error {
	if gatewayID == uuid.Nil {
		return errors.New("runtime identity is invalid")
	}
	credentials, err := newWIFCredentials(
		certificateConfigPath,
		runtimeCloudScopes,
		true,
		&http.Client{Timeout: authHTTPTimeout},
	)
	if err != nil {
		return errors.New("runtime identity authentication failed")
	}
	operationCtx, cancel := context.WithTimeout(ctx, authHTTPTimeout)
	defer cancel()
	if _, err := credentials.Token(operationCtx); err != nil {
		return errors.New("runtime identity authentication failed")
	}
	return nil
}

func reusableOAuth2TokenSource(credentials *auth.Credentials) oauth2.TokenSource {
	return oauth2.ReuseTokenSource(
		nil,
		oauth2adapt.TokenSourceFromTokenProvider(credentials.TokenProvider),
	)
}

type renewalRequest struct {
	GatewayID             string `json:"gateway_id"`
	CurrentCertificatePEM string `json:"current_certificate_pem"`
	CSRPEM                string `json:"csr_pem"`
}

type renewalResponse struct {
	GatewayID      string `json:"gateway_id"`
	CertificatePEM string `json:"certificate_pem"`
}

func renewGatewayCertificate(
	ctx context.Context,
	credentials *runtimeCredentials,
	now time.Time,
) (bool, error) {
	if credentials == nil || credentials.renewalTokenSource == nil {
		return false, errors.New("runtime renewal credentials are unavailable")
	}
	identity, err := loadGatewayIdentity(now)
	if err != nil {
		return false, err
	}
	paths, err := resolveIdentityPaths()
	if err != nil {
		return false, err
	}
	directoryGeneration, err := captureIdentityDirectoryGeneration(paths)
	if err != nil {
		return false, err
	}
	if identity.gatewayID != credentials.gatewayID {
		return false, errors.New("runtime renewal identity changed unexpectedly")
	}
	if !gatewayCertificateNeedsRenewal(identity.certificate, now) {
		return false, nil
	}
	csrPEM, err := createGatewayCertificateRequest(identity)
	if err != nil {
		return false, err
	}
	client := oauth2.NewClient(ctx, credentials.renewalTokenSource)
	client.Timeout = authHTTPTimeout
	response, err := requestCertificateRenewal(
		ctx,
		client,
		commissionServiceURL+"/renew",
		identity,
		csrPEM,
	)
	if err != nil {
		return false, err
	}
	if response.GatewayID != identity.gatewayID.String() {
		return false, errors.New("renewal service returned an invalid GatewayID")
	}
	candidatePEM := []byte(response.CertificatePEM)
	if _, err := validateGatewayCertificate(
		candidatePEM,
		identity.privateKey,
		identity.gatewayID,
		now,
	); err != nil {
		return false, errors.New("renewal service returned an invalid certificate")
	}
	release, err := acquireGatewayLifecycleLock(lifecycleOperationLockWait)
	if err != nil {
		return false, err
	}
	defer release()
	if err := ensureCommissioningAllowed(paths); err != nil {
		return false, err
	}
	currentGeneration, err := captureIdentityDirectoryGeneration(paths)
	if err != nil || !os.SameFile(directoryGeneration, currentGeneration) {
		return false, errors.New("runtime renewal identity generation changed unexpectedly")
	}
	currentIdentity, err := loadGatewayIdentity(time.Now().UTC())
	if err != nil || !sameGatewayIdentity(identity, currentIdentity) {
		return false, errors.New("runtime renewal identity changed unexpectedly")
	}
	// The proof uses fixed, protected staging names. Keep the lifecycle lock
	// through proof and publication so terminal removal, commissioning, and a
	// competing renewal cannot redirect those writes into another generation.
	if err := proveRenewedCertificate(ctx, paths, candidatePEM); err != nil {
		return false, err
	}
	if err := saveGatewayCertificate(paths, candidatePEM); err != nil {
		return false, errors.New("renewed Gateway certificate persistence failed")
	}
	return true, nil
}

func sameGatewayIdentity(first, second gatewayIdentity) bool {
	return first.gatewayID != uuid.Nil && first.gatewayID == second.gatewayID &&
		first.privateKey != nil && second.privateKey != nil &&
		publicKeysEqual(&first.privateKey.PublicKey, &second.privateKey.PublicKey) &&
		bytes.Equal(first.certificatePEM, second.certificatePEM)
}

func requestCertificateRenewal(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	identity gatewayIdentity,
	csrPEM []byte,
) (renewalResponse, error) {
	requestBody, err := json.Marshal(renewalRequest{
		GatewayID:             identity.gatewayID.String(),
		CurrentCertificatePEM: string(identity.certificatePEM),
		CSRPEM:                string(csrPEM),
	})
	if err != nil {
		return renewalResponse{}, errors.New("certificate renewal request failed")
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		endpoint,
		bytes.NewReader(requestBody),
	)
	if err != nil {
		return renewalResponse{}, errors.New("certificate renewal request failed")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return renewalResponse{}, errors.New("certificate renewal service unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return renewalResponse{}, errors.New("certificate renewal service rejected the request")
	}
	encoded, err := io.ReadAll(io.LimitReader(response.Body, commissionResponseMaxBytes+1))
	if err != nil || len(encoded) > commissionResponseMaxBytes {
		return renewalResponse{}, errors.New("certificate renewal response is invalid")
	}
	var result renewalResponse
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return renewalResponse{}, errors.New("certificate renewal response is invalid")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return renewalResponse{}, errors.New("certificate renewal response is invalid")
	}
	if result.GatewayID == "" || result.CertificatePEM == "" {
		return renewalResponse{}, errors.New("certificate renewal response is invalid")
	}
	return result, nil
}

func proveRenewedCertificate(
	ctx context.Context,
	paths identityPaths,
	candidatePEM []byte,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	candidateCertificatePath := filepath.Join(paths.directory, ".certificate-renewing.pem")
	candidateConfigPath := filepath.Join(paths.directory, ".certificate-config-renewing.json")
	defer func() {
		_ = os.Remove(candidateCertificatePath)
		_ = os.Remove(candidateConfigPath)
	}()
	if err := os.Remove(candidateCertificatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("renewed Gateway certificate staging failed")
	}
	if err := os.Remove(candidateConfigPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("renewed Gateway certificate staging failed")
	}
	candidatePaths := paths
	candidatePaths.certificate = candidateCertificatePath
	candidatePaths.certificateConfig = candidateConfigPath
	if err := atomicWriteIdentityFile(
		candidatePaths,
		candidateCertificatePath,
		candidatePEM,
		true,
	); err != nil {
		return errors.New("renewed Gateway certificate staging failed")
	}
	if err := writeCertificateConfig(
		candidatePaths,
		candidateCertificatePath,
		paths.privateKey,
	); err != nil {
		return errors.New("renewed Gateway certificate staging failed")
	}
	probeCredentials, err := newWIFCredentials(
		candidateConfigPath,
		[]string{cloudPlatformScope},
		true,
		&http.Client{Timeout: authHTTPTimeout},
	)
	if err != nil {
		return errors.New("renewed Gateway certificate authentication failed")
	}
	if _, err := probeCredentials.Token(ctx); err != nil {
		return errors.New("renewed Gateway certificate authentication failed")
	}
	return nil
}
