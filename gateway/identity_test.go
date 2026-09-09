package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestGatewayPrivateKeyRoundTripIsRSA2048(t *testing.T) {
	privateKey, err := generateGatewayPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	if privateKey.N.BitLen() != 2048 {
		t.Fatalf("key size = %d", privateKey.N.BitLen())
	}
	encoded, err := marshalGatewayPrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseGatewayPrivateKey(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !publicKeysEqual(&privateKey.PublicKey, &parsed.PublicKey) {
		t.Fatal("round trip changed the public key")
	}
	publicPEM, err := marshalGatewayPublicKey(parsed)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(publicPEM)
	if block == nil || block.Type != "PUBLIC KEY" || len(rest) != 0 {
		t.Fatal("public key PEM is invalid")
	}
}

func TestGatewayPrivateKeyRejectsWrongSizeAndTrailingData(t *testing.T) {
	wrongSize, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	wrongDER, err := x509.MarshalPKCS8PrivateKey(wrongSize)
	if err != nil {
		t.Fatal(err)
	}
	wrongPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: wrongDER})
	if _, err := parseGatewayPrivateKey(wrongPEM); err == nil {
		t.Fatal("RSA-1024 key was accepted")
	}
	privateKey, err := generateGatewayPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := marshalGatewayPrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseGatewayPrivateKey(append(encoded, []byte("unexpected")...)); err == nil {
		t.Fatal("trailing private-key data was accepted")
	}
}

func TestGatewayCertificateValidationBindsUUIDKeyAndLifetime(t *testing.T) {
	now := time.Date(2026, 8, 4, 11, 38, 55, 0, time.UTC)
	gatewayID := uuid.MustParse("1d91378f-7b96-4e6f-95b2-1304b728d28f")
	privateKey, err := generateGatewayPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := makeTestCertificate(
		t,
		privateKey,
		gatewayID.String(),
		now.Add(-time.Hour),
		now.Add(365*24*time.Hour-time.Hour),
	)
	certificate, err := validateGatewayCertificate(certificatePEM, privateKey, gatewayID, now)
	if err != nil || certificate.Subject.CommonName != gatewayID.String() {
		t.Fatalf("valid certificate rejected: %v", err)
	}

	otherKey, err := generateGatewayPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validateGatewayCertificate(certificatePEM, otherKey, gatewayID, now); err == nil {
		t.Fatal("certificate/key mismatch was accepted")
	}
	if _, err := validateGatewayCertificate(certificatePEM, privateKey, uuid.New(), now); err == nil {
		t.Fatal("certificate/GatewayID mismatch was accepted")
	}
	expired := makeTestCertificate(t, privateKey, gatewayID.String(), now.Add(-365*24*time.Hour), now.Add(-time.Second))
	if _, err := validateGatewayCertificate(expired, privateKey, gatewayID, now); err == nil {
		t.Fatal("expired certificate was accepted")
	}
	tooShort := makeTestCertificate(t, privateKey, gatewayID.String(), now.Add(-time.Hour), now.Add(100*24*time.Hour))
	if _, err := validateGatewayCertificate(tooShort, privateKey, gatewayID, now); err == nil {
		t.Fatal("short-lived certificate was accepted")
	}
}

func TestCertificateRenewalWindowBoundary(t *testing.T) {
	now := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	certificate := &x509.Certificate{NotBefore: now.Add(-300 * 24 * time.Hour), NotAfter: now.Add(30 * 24 * time.Hour)}
	if !gatewayCertificateNeedsRenewal(certificate, now) {
		t.Fatal("certificate at the 30-day boundary was not renewable")
	}
	certificate.NotAfter = certificate.NotAfter.Add(time.Nanosecond)
	if gatewayCertificateNeedsRenewal(certificate, now) {
		t.Fatal("certificate outside the renewal window was renewable")
	}
}

func TestCommissionHashIsDeterministicLowercaseSHA256(t *testing.T) {
	raw := "commission-secret-value"
	first := hashCommissionID(raw)
	second := hashCommissionID(raw)
	if !first.matches(second) {
		t.Fatal("same Commission ID produced different hashes")
	}
	encoded := string(first.encoded())
	if len(encoded) != 64 || encoded != strings.ToLower(encoded) || strings.Contains(encoded, raw) {
		t.Fatalf("invalid encoded commission hash %q", encoded)
	}
	parsed, err := parseCommissionHash([]byte(encoded))
	if err != nil || !parsed.matches(first) {
		t.Fatalf("commission hash round trip failed: %v", err)
	}
	if _, err := parseCommissionHash([]byte(strings.ToUpper(encoded))); err == nil {
		t.Fatal("non-canonical commission hash was accepted")
	}
}

func TestCertificateConfigurationUsesOnlyAbsoluteKeyAndCertificatePaths(t *testing.T) {
	certificatePath := filepath.Join(t.TempDir(), "certificate.pem")
	privateKeyPath := filepath.Join(filepath.Dir(certificatePath), "private-key.pem")
	encoded, err := marshalCertificateConfig(certificatePath, privateKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		CertConfigs struct {
			Workload struct {
				CertificatePath string `json:"cert_path"`
				PrivateKeyPath  string `json:"key_path"`
			} `json:"workload"`
		} `json:"cert_configs"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.CertConfigs.Workload.CertificatePath != certificatePath ||
		decoded.CertConfigs.Workload.PrivateKeyPath != privateKeyPath ||
		strings.Contains(string(encoded), "token") {
		t.Fatalf("certificate configuration has the wrong contents: %s", encoded)
	}
	if _, err := marshalCertificateConfig("relative.pem", privateKeyPath); err == nil {
		t.Fatal("relative certificate path was accepted")
	}
}

func TestDurableIdentityEntryRejectsReplacementOrAbsence(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "commit-record")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := requireDurableIdentityEntry(directory, path, original); err != nil {
		t.Fatalf("unchanged entry was rejected: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := requireDurableIdentityEntry(directory, path, original); err == nil {
		t.Fatal("replacement entry was accepted as the original commit record")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := requireDurableIdentityEntry(directory, path, nil); err == nil {
		t.Fatal("absent commit record was accepted as durable")
	}
}

func makeTestCertificate(
	t *testing.T,
	privateKey *rsa.PrivateKey,
	commonName string,
	notBefore time.Time,
	notAfter time.Time,
) []byte {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(
		rand.Reader,
		template,
		template,
		&privateKey.PublicKey,
		privateKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
