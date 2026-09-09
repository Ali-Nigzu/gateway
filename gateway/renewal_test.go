package main

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRenewalCSRReusesGatewayPrivateKey(t *testing.T) {
	privateKey, err := generateGatewayPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	gatewayID := uuid.MustParse("1d91378f-7b96-4e6f-95b2-1304b728d28f")
	encoded, err := createGatewayCertificateRequest(gatewayIdentity{
		gatewayID:  gatewayID,
		privateKey: privateKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(encoded)
	if block == nil || block.Type != "CERTIFICATE REQUEST" || len(rest) != 0 {
		t.Fatal("renewal CSR PEM is invalid")
	}
	request, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil || request.CheckSignature() != nil {
		t.Fatalf("renewal CSR signature is invalid: %v", err)
	}
	publicKey, ok := request.PublicKey.(*rsa.PublicKey)
	if !ok || !publicKeysEqual(publicKey, &privateKey.PublicKey) {
		t.Fatal("renewal CSR did not reuse the durable private key")
	}
	if request.Subject.CommonName != gatewayID.String() {
		t.Fatalf("CSR CN = %q", request.Subject.CommonName)
	}
}

func TestRenewalRequestUsesLockedWireContract(t *testing.T) {
	now := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)
	gatewayID := uuid.MustParse("1d91378f-7b96-4e6f-95b2-1304b728d28f")
	privateKey, err := generateGatewayPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := makeTestCertificate(t, privateKey, gatewayID.String(), now.Add(-time.Hour), now.Add(365*24*time.Hour-time.Hour))
	identity := gatewayIdentity{
		gatewayID:      gatewayID,
		privateKey:     privateKey,
		certificatePEM: certificatePEM,
	}
	csr, err := createGatewayCertificateRequest(identity)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var received map[string]string
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Error(err)
		}
		if len(received) != 3 || received["gateway_id"] != gatewayID.String() ||
			received["current_certificate_pem"] != string(certificatePEM) ||
			received["csr_pem"] != string(csr) {
			t.Errorf("unexpected renewal request: %#v", received)
		}
		fmt.Fprintf(writer, `{"gateway_id":%q,"certificate_pem":"replacement"}`, gatewayID.String())
	}))
	defer server.Close()
	response, err := requestCertificateRenewal(
		context.Background(),
		server.Client(),
		server.URL,
		identity,
		csr,
	)
	if err != nil || response.GatewayID != gatewayID.String() || response.CertificatePEM != "replacement" {
		t.Fatalf("renewal response = %#v, %v", response, err)
	}
}

func TestRenewalCommitRequiresUnchangedGatewayIdentity(t *testing.T) {
	privateKey, err := generateGatewayPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	otherPrivateKey, err := generateGatewayPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	gatewayID := uuid.MustParse("1d91378f-7b96-4e6f-95b2-1304b728d28f")
	expected := gatewayIdentity{
		gatewayID:      gatewayID,
		privateKey:     privateKey,
		certificatePEM: []byte("current-certificate"),
	}
	if !sameGatewayIdentity(expected, expected) {
		t.Fatal("unchanged renewal identity was rejected")
	}
	for name, changed := range map[string]gatewayIdentity{
		"GatewayID": {
			gatewayID:      uuid.MustParse("673fa485-1838-46a8-b283-29d109260ca6"),
			privateKey:     privateKey,
			certificatePEM: expected.certificatePEM,
		},
		"private key": {
			gatewayID:      gatewayID,
			privateKey:     otherPrivateKey,
			certificatePEM: expected.certificatePEM,
		},
		"certificate": {
			gatewayID:      gatewayID,
			privateKey:     privateKey,
			certificatePEM: []byte("replacement-current-certificate"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			if sameGatewayIdentity(expected, changed) {
				t.Fatal("changed renewal identity was accepted")
			}
		})
	}
}
