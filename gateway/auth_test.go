package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestBootstrapCredentialValidationRequiresExactIdentity(t *testing.T) {
	valid := map[string]string{
		"type":         "service_account",
		"project_id":   runtimeProjectID,
		"client_email": bootstrapServiceAccount,
		"private_key":  "private material",
		"token_uri":    "https://oauth2.googleapis.com/token",
	}
	encoded, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateBootstrapCredentials(encoded); err != nil {
		t.Fatalf("valid metadata rejected: %v", err)
	}
	for field, value := range map[string]string{
		"type":         "authorized_user",
		"project_id":   "another-project",
		"client_email": "gateway-runtime@camosbase.iam.gserviceaccount.com",
		"private_key":  "",
		"token_uri":    "http://oauth2.googleapis.com/token",
	} {
		invalid := make(map[string]string, len(valid))
		for key, original := range valid {
			invalid[key] = original
		}
		invalid[field] = value
		encoded, err := json.Marshal(invalid)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateBootstrapCredentials(encoded); err == nil {
			t.Fatalf("invalid %s was accepted", field)
		}
	}
}

func TestRuntimeIdentityFenceRejectsReplacementGeneration(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "identity")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := newIdentityPaths(directory, filepath.Join(directory, "GatewayID"))
	gatewayID := uuid.MustParse("1d91378f-7b96-4e6f-95b2-1304b728d28f")
	if err := os.WriteFile(paths.gatewayID, []byte(gatewayID.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	generation, err := captureIdentityDirectoryGeneration(paths)
	if err != nil {
		t.Fatal(err)
	}
	readID := func() (uuid.UUID, error) { return readRemovalGatewayID(paths.gatewayID) }
	if err := validateBoundIdentityGeneration(generation, gatewayID, paths, readID); err != nil {
		t.Fatalf("current runtime identity generation was rejected: %v", err)
	}
	if err := os.Rename(directory, directory+".retired"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.gatewayID, []byte(gatewayID.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateBoundIdentityGeneration(generation, gatewayID, paths, readID); err == nil {
		t.Fatal("replacement directory with the same GatewayID passed the runtime identity fence")
	}
}

func TestRuntimeWIFOptionsAreExact(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), certificateConfigName)
	options := runtimeWIFOptions(
		configPath,
		[]string{cloudPlatformScope, cloudSQLAdminScope},
		true,
		&http.Client{},
	)
	if options.Audience != runtimeWIFAudience ||
		options.SubjectTokenType != runtimeWIFSubjectTokenType ||
		options.TokenURL != runtimeWIFTokenURL {
		t.Fatalf("unexpected WIF identity contract: %#v", options)
	}
	if options.CredentialSource == nil || options.CredentialSource.Certificate == nil ||
		options.CredentialSource.Certificate.CertificateConfigLocation != configPath {
		t.Fatal("WIF certificate source does not use the durable config path")
	}
	if options.ServiceAccountImpersonationURL != runtimeImpersonationURL ||
		options.ServiceAccountImpersonationLifetimeSeconds != 3600 {
		t.Fatal("runtime service-account impersonation is invalid")
	}
	if !reflect.DeepEqual(options.Scopes, []string{cloudPlatformScope, cloudSQLAdminScope}) {
		t.Fatalf("runtime scopes = %#v", options.Scopes)
	}
	direct := runtimeWIFOptions(configPath, []string{cloudPlatformScope}, false, &http.Client{})
	if direct.ServiceAccountImpersonationURL != "" {
		t.Fatal("direct WIF credentials unexpectedly impersonate a service account")
	}
}

func TestRuntimeCredentialConstructionDoesNotNeedLooseServiceAccountJSON(t *testing.T) {
	directory := t.TempDir()
	certificatePath := filepath.Join(directory, certificateFilename)
	privateKeyPath := filepath.Join(directory, privateKeyFilename)
	configPath := filepath.Join(directory, certificateConfigName)
	config, err := marshalCertificateConfig(certificatePath, privateKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, config, 0o600); err != nil {
		t.Fatal(err)
	}
	credentials, err := newRuntimeCredentialsFromConfig(
		context.Background(),
		uuid.MustParse("1d91378f-7b96-4e6f-95b2-1304b728d28f"),
		configPath,
		&http.Client{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if credentials.cloudPlatformTokenSource == nil ||
		credentials.databaseLoginTokenSource == nil ||
		credentials.renewalTokenSource == nil {
		t.Fatal("runtime token sources were not constructed")
	}
}
