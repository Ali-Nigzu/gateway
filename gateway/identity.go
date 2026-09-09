package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	privateKeyFilename       = "private-key.pem"
	certificateFilename      = "certificate.pem"
	certificateConfigName    = "certificate-config.json"
	commissionHashFilename   = "commission-hash"
	removalPendingFilename   = "removal.pending"
	updatePendingFilename    = "update.pending"
	lifecycleStatusFilename  = "lifecycle.status"
	identityWorkDirectory    = "work"
	minimumCertificatePeriod = 364 * 24 * time.Hour
	maximumCertificatePeriod = 366 * 24 * time.Hour
	certificateRenewalWindow = 30 * 24 * time.Hour
)

// errIdentityPublishedDurabilityUnknown is returned only after the target
// pathname has been atomically published and its durability cannot be proven.
// A create-only commit caller may recover only by re-reading the exact record
// through a path that successfully resynchronizes and revalidates the entry.
var errIdentityPublishedDurabilityUnknown = errors.New(
	"identity state was published but its directory sync failed",
)

type identityPaths struct {
	directory         string
	gatewayID         string
	privateKey        string
	certificate       string
	certificateConfig string
	commissionHash    string
	removalPending    string
	updatePending     string
	lifecycleStatus   string
	workDirectory     string
}

func newIdentityPaths(directory, gatewayIDPath string) identityPaths {
	return identityPaths{
		directory:         directory,
		gatewayID:         gatewayIDPath,
		privateKey:        filepath.Join(directory, privateKeyFilename),
		certificate:       filepath.Join(directory, certificateFilename),
		certificateConfig: filepath.Join(directory, certificateConfigName),
		commissionHash:    filepath.Join(directory, commissionHashFilename),
		removalPending:    filepath.Join(directory, removalPendingFilename),
		updatePending:     filepath.Join(directory, updatePendingFilename),
		lifecycleStatus:   filepath.Join(directory, lifecycleStatusFilename),
		workDirectory:     filepath.Join(directory, identityWorkDirectory),
	}
}

func captureIdentityDirectoryGeneration(paths identityPaths) (os.FileInfo, error) {
	info, err := os.Lstat(paths.directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Gateway identity directory is invalid")
	}
	return info, nil
}

type commissionHash [sha256.Size]byte

func hashCommissionID(raw string) commissionHash {
	return sha256.Sum256([]byte(raw))
}

func (hash commissionHash) matches(other commissionHash) bool {
	return subtle.ConstantTimeCompare(hash[:], other[:]) == 1
}

func (hash commissionHash) encoded() []byte {
	encoded := make([]byte, hex.EncodedLen(len(hash)))
	hex.Encode(encoded, hash[:])
	return encoded
}

func parseCommissionHash(encoded []byte) (commissionHash, error) {
	var hash commissionHash
	if len(encoded) != hex.EncodedLen(len(hash)) ||
		string(encoded) != strings.ToLower(string(encoded)) {
		return hash, errors.New("pending commissioning state is invalid")
	}
	decoded, err := hex.DecodeString(string(encoded))
	if err != nil || len(decoded) != len(hash) {
		return hash, errors.New("pending commissioning state is invalid")
	}
	copy(hash[:], decoded)
	return hash, nil
}

type gatewayIdentity struct {
	gatewayID      uuid.UUID
	privateKey     *rsa.PrivateKey
	certificate    *x509.Certificate
	certificatePEM []byte
}

func generateGatewayPrivateKey() (*rsa.PrivateKey, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, errors.New("Gateway private key generation failed")
	}
	if err := privateKey.Validate(); err != nil {
		return nil, errors.New("Gateway private key generation failed")
	}
	return privateKey, nil
}

func marshalGatewayPrivateKey(privateKey *rsa.PrivateKey) ([]byte, error) {
	if err := validateGatewayPrivateKey(privateKey); err != nil {
		return nil, err
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, errors.New("Gateway private key encoding failed")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded}), nil
}

func parseGatewayPrivateKey(encoded []byte) (*rsa.PrivateKey, error) {
	block, rest := pem.Decode(encoded)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("Gateway private key is invalid")
	}

	var parsed any
	var err error
	switch block.Type {
	case "PRIVATE KEY":
		parsed, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	case "RSA PRIVATE KEY":
		parsed, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	default:
		return nil, errors.New("Gateway private key is invalid")
	}
	if err != nil {
		return nil, errors.New("Gateway private key is invalid")
	}
	privateKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("Gateway private key is not RSA")
	}
	if err := validateGatewayPrivateKey(privateKey); err != nil {
		return nil, err
	}
	return privateKey, nil
}

func validateGatewayPrivateKey(privateKey *rsa.PrivateKey) error {
	if privateKey == nil || privateKey.N == nil || privateKey.N.BitLen() != 2048 {
		return errors.New("Gateway private key must be RSA-2048")
	}
	if err := privateKey.Validate(); err != nil {
		return errors.New("Gateway private key is invalid")
	}
	return nil
}

func marshalGatewayPublicKey(privateKey *rsa.PrivateKey) ([]byte, error) {
	if err := validateGatewayPrivateKey(privateKey); err != nil {
		return nil, err
	}
	encoded, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return nil, errors.New("Gateway public key encoding failed")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: encoded}), nil
}

func parseCertificateChain(encoded []byte) (*x509.Certificate, error) {
	rest := encoded
	var leaf *x509.Certificate
	for {
		block, remaining := pem.Decode(rest)
		if block == nil {
			if len(bytes.TrimSpace(rest)) != 0 {
				return nil, errors.New("Gateway certificate is invalid")
			}
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, errors.New("Gateway certificate is invalid")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, errors.New("Gateway certificate is invalid")
		}
		if leaf == nil {
			leaf = certificate
		}
		rest = remaining
	}
	if leaf == nil {
		return nil, errors.New("Gateway certificate is invalid")
	}
	return leaf, nil
}

func validateGatewayCertificate(
	encoded []byte,
	privateKey *rsa.PrivateKey,
	gatewayID uuid.UUID,
	now time.Time,
) (*x509.Certificate, error) {
	if gatewayID == uuid.Nil {
		return nil, errors.New("GatewayID is invalid")
	}
	if err := validateGatewayPrivateKey(privateKey); err != nil {
		return nil, err
	}
	certificate, err := parseCertificateChain(encoded)
	if err != nil {
		return nil, err
	}
	certificatePublicKey, ok := certificate.PublicKey.(*rsa.PublicKey)
	if !ok || !publicKeysEqual(certificatePublicKey, &privateKey.PublicKey) {
		return nil, errors.New("Gateway certificate does not match the private key")
	}
	if certificate.Subject.CommonName != gatewayID.String() {
		return nil, errors.New("Gateway certificate identity is invalid")
	}
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return nil, errors.New("Gateway certificate is not currently valid")
	}
	lifetime := certificate.NotAfter.Sub(certificate.NotBefore)
	if lifetime < minimumCertificatePeriod || lifetime > maximumCertificatePeriod {
		return nil, errors.New("Gateway certificate lifetime is invalid")
	}
	return certificate, nil
}

func publicKeysEqual(first, second *rsa.PublicKey) bool {
	return first != nil && second != nil && first.N != nil && second.N != nil &&
		first.E == second.E && first.N.Cmp(second.N) == 0
}

func loadGatewayIdentity(now time.Time) (gatewayIdentity, error) {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return gatewayIdentity{}, err
	}
	gatewayID, err := loadGatewayID()
	if err != nil {
		return gatewayIdentity{}, err
	}
	privateKeyPEM, err := os.ReadFile(paths.privateKey)
	if err != nil {
		return gatewayIdentity{}, errors.New("Gateway private key is unavailable")
	}
	privateKey, err := parseGatewayPrivateKey(privateKeyPEM)
	if err != nil {
		return gatewayIdentity{}, err
	}
	certificatePEM, err := os.ReadFile(paths.certificate)
	if err != nil {
		return gatewayIdentity{}, errors.New("Gateway certificate is unavailable")
	}
	certificate, err := validateGatewayCertificate(certificatePEM, privateKey, gatewayID, now)
	if err != nil {
		return gatewayIdentity{}, err
	}
	return gatewayIdentity{
		gatewayID:      gatewayID,
		privateKey:     privateKey,
		certificate:    certificate,
		certificatePEM: certificatePEM,
	}, nil
}

func gatewayCertificateNeedsRenewal(certificate *x509.Certificate, now time.Time) bool {
	if certificate == nil || now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return false
	}
	return !now.Add(certificateRenewalWindow).Before(certificate.NotAfter)
}

func createGatewayCertificateRequest(identity gatewayIdentity) ([]byte, error) {
	if identity.gatewayID == uuid.Nil || identity.privateKey == nil {
		return nil, errors.New("Gateway identity is invalid")
	}
	request, err := x509.CreateCertificateRequest(
		rand.Reader,
		&x509.CertificateRequest{Subject: pkix.Name{CommonName: identity.gatewayID.String()}},
		identity.privateKey,
	)
	if err != nil {
		return nil, errors.New("Gateway certificate renewal request failed")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: request}), nil
}

func ensureCertificateConfig(paths identityPaths) error {
	absoluteCertificate, err := filepath.Abs(paths.certificate)
	if err != nil {
		return errors.New("Gateway certificate path is invalid")
	}
	absolutePrivateKey, err := filepath.Abs(paths.privateKey)
	if err != nil {
		return errors.New("Gateway private key path is invalid")
	}
	return writeCertificateConfig(paths, absoluteCertificate, absolutePrivateKey)
}

func writeCertificateConfig(paths identityPaths, certificatePath, privateKeyPath string) error {
	encoded, err := marshalCertificateConfig(certificatePath, privateKeyPath)
	if err != nil {
		return err
	}
	if err := atomicWriteIdentityFile(paths, paths.certificateConfig, encoded, true); err != nil {
		return fmt.Errorf("Gateway certificate configuration failed: %w", err)
	}
	return nil
}

func marshalCertificateConfig(certificatePath, privateKeyPath string) ([]byte, error) {
	if !filepath.IsAbs(certificatePath) || !filepath.IsAbs(privateKeyPath) {
		return nil, errors.New("Gateway certificate configuration paths must be absolute")
	}
	configuration := struct {
		CertConfigs struct {
			Workload struct {
				CertificatePath string `json:"cert_path"`
				PrivateKeyPath  string `json:"key_path"`
			} `json:"workload"`
		} `json:"cert_configs"`
	}{}
	configuration.CertConfigs.Workload.CertificatePath = certificatePath
	configuration.CertConfigs.Workload.PrivateKeyPath = privateKeyPath
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return nil, errors.New("Gateway certificate configuration failed")
	}
	encoded = append(encoded, '\n')
	return encoded, nil
}

func loadPendingCommissionHash(paths identityPaths) (*commissionHash, error) {
	encoded, err := os.ReadFile(paths.commissionHash)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("pending commissioning state is unavailable")
	}
	hash, err := parseCommissionHash(encoded)
	if err != nil {
		return nil, err
	}
	return &hash, nil
}

func removePendingCommissionHash() error {
	paths, err := resolveIdentityPaths()
	if err != nil {
		return err
	}
	if err := os.Remove(paths.commissionHash); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("pending commissioning state removal failed")
	}
	return syncIdentityDirectory(paths.directory)
}

func atomicWriteIdentityFile(
	paths identityPaths,
	targetPath string,
	data []byte,
	replace bool,
) error {
	if filepath.Clean(filepath.Dir(targetPath)) != filepath.Clean(paths.directory) {
		return errors.New("identity state path is outside the identity directory")
	}
	if err := prepareIdentityDirectory(paths); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(paths.directory, ".identity-installing-")
	if err != nil {
		return errors.New("identity state temporary file creation failed")
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		_ = temporary.Close()
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := secureIdentityFile(temporaryPath); err != nil {
		return err
	}
	written, err := temporary.Write(data)
	if err != nil || written != len(data) {
		return errors.New("identity state write failed")
	}
	if err := temporary.Sync(); err != nil {
		return errors.New("identity state sync failed")
	}
	if err := temporary.Close(); err != nil {
		return errors.New("identity state close failed")
	}
	published, err := publishIdentityFile(temporaryPath, targetPath, replace)
	if err != nil {
		return err
	}
	if !published {
		return os.ErrExist
	}
	keepTemporary = false
	if err := requireDurableIdentityEntry(paths.directory, targetPath, nil); err != nil {
		return fmt.Errorf("%w: %v", errIdentityPublishedDurabilityUnknown, err)
	}
	return nil
}

// requireDurableIdentityEntry makes a pathname usable as commit authority only
// after the containing directory is synchronized and the expected regular-file
// entry is still the one published/read. Windows publication supplies the
// durability guarantee through MOVEFILE_WRITE_THROUGH; its directory sync is a
// no-op, while the same pathname validation still applies.
func requireDurableIdentityEntry(
	directory string,
	targetPath string,
	expected os.FileInfo,
) error {
	if filepath.Clean(filepath.Dir(targetPath)) != filepath.Clean(directory) {
		return errors.New("identity state path is outside the identity directory")
	}
	if err := syncIdentityDirectory(directory); err != nil {
		return err
	}
	actual, err := os.Lstat(targetPath)
	if err != nil || !actual.Mode().IsRegular() {
		return errors.New("published identity state is unavailable")
	}
	if expected != nil && !os.SameFile(expected, actual) {
		return errors.New("published identity state changed during inspection")
	}
	return nil
}

func removeUncommittedIdentity(paths identityPaths) error {
	for _, path := range []string{
		paths.privateKey,
		paths.certificate,
		paths.certificateConfig,
		paths.commissionHash,
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("incomplete Gateway identity cleanup failed")
		}
	}
	return syncIdentityDirectory(paths.directory)
}

func readGatewayPrivateKey(path string) (*rsa.PrivateKey, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("Gateway private key is unavailable")
	}
	return parseGatewayPrivateKey(encoded)
}

func saveNewGatewayPrivateKey(paths identityPaths, privateKey *rsa.PrivateKey) (bool, error) {
	encoded, err := marshalGatewayPrivateKey(privateKey)
	if err != nil {
		return false, err
	}
	err = atomicWriteIdentityFile(paths, paths.privateKey, encoded, false)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	return err == nil, err
}

func saveNewCommissionHash(paths identityPaths, hash commissionHash) (bool, error) {
	err := atomicWriteIdentityFile(paths, paths.commissionHash, hash.encoded(), false)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	return err == nil, err
}

func saveGatewayCertificate(paths identityPaths, certificatePEM []byte) error {
	return atomicWriteIdentityFile(paths, paths.certificate, certificatePEM, true)
}
