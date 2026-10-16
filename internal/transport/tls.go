package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jhh3/pyre/internal/fileperm"
)

// Hub certificate file names inside the directory given to EnsureHubCert.
const (
	HubCertFile = "hub-cert.pem"
	HubKeyFile  = "hub-key.pem"
)

// HubCert is the hub's TLS identity.
type HubCert struct {
	CertFile string
	KeyFile  string
	// Fingerprint is the lowercase hex SHA-256 of the leaf certificate DER.
	// Clients pin it (EnrollmentTicket.HubCertSHA256, pairing URI "pin").
	Fingerprint string
	Certificate tls.Certificate
	Leaf        *x509.Certificate
}

// EnsureHubCert loads the hub certificate from dir, or generates and
// persists a self-signed ECDSA P-256 certificate (key file mode 0600, dir
// 0700) when none exists. An existing certificate is always reused so the
// pinned fingerprint stays stable; hosts only affect a newly generated
// certificate's SANs (pinned clients do not check names). Empty hosts
// default to localhost, 127.0.0.1, ::1 and the machine hostname.
//
// It refuses to proceed when only one of the two files exists or the key
// file is group/world accessible, rather than silently minting a new pin.
func EnsureHubCert(dir string, hosts []string) (*HubCert, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("transport: create cert dir: %w", err)
	}
	certPath := filepath.Join(dir, HubCertFile)
	keyPath := filepath.Join(dir, HubKeyFile)
	_, certErr := os.Stat(certPath)
	_, keyErr := os.Stat(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		return loadHubCert(certPath, keyPath)
	case errors.Is(certErr, fs.ErrNotExist) && errors.Is(keyErr, fs.ErrNotExist):
		if err := generateHubCert(certPath, keyPath, hosts); err != nil {
			return nil, err
		}
		return loadHubCert(certPath, keyPath)
	case certErr != nil && !errors.Is(certErr, fs.ErrNotExist):
		return nil, fmt.Errorf("transport: stat hub cert: %w", certErr)
	case keyErr != nil && !errors.Is(keyErr, fs.ErrNotExist):
		return nil, fmt.Errorf("transport: stat hub key: %w", keyErr)
	default:
		return nil, fmt.Errorf("transport: only one of %s and %s exists in %s; restore the missing file or remove both to mint a new certificate (all nodes and clients must then re-pin)", HubCertFile, HubKeyFile, dir)
	}
}

func generateHubCert(certPath, keyPath string, hosts []string) error {
	if len(hosts) == 0 {
		hosts = []string{"localhost", "127.0.0.1", "::1"}
		if hn, err := os.Hostname(); err == nil && hn != "" {
			hosts = append(hosts, hn)
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "pyre-hub", Organization: []string{"Pyre"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := WriteFileAtomic(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return WriteFileAtomic(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func loadHubCert(certPath, keyPath string) (*HubCert, error) {
	st, err := os.Stat(keyPath)
	if err != nil {
		return nil, err
	}
	if desc, bad := fileperm.Exposed(keyPath, st); bad {
		return nil, fmt.Errorf("transport: hub key %s has %s; %s", keyPath, desc, fileperm.FixHint(keyPath, false))
	}
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("transport: load hub certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	pair.Leaf = leaf
	return &HubCert{
		CertFile:    certPath,
		KeyFile:     keyPath,
		Fingerprint: CertFingerprint(pair.Certificate[0]),
		Certificate: pair,
		Leaf:        leaf,
	}, nil
}

// ServerTLSConfig returns a TLS server configuration presenting this
// certificate.
func (c *HubCert) ServerTLSConfig() *tls.Config {
	return &tls.Config{Certificates: []tls.Certificate{c.Certificate}, MinVersion: tls.VersionTLS12}
}

// CertFingerprint returns the lowercase hex SHA-256 of a DER certificate.
func CertFingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// NormalizeFingerprint accepts "sha256:AB:CD...", "AB:CD...", or plain hex
// (any case) and returns 64 lowercase hex characters.
func NormalizeFingerprint(s string) (string, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	s = strings.TrimPrefix(s, "sha256:")
	s = strings.ReplaceAll(s, ":", "")
	if len(s) != 64 {
		return "", fmt.Errorf("transport: certificate fingerprint must be 64 hex characters (SHA-256), got %d", len(s))
	}
	if _, err := hex.DecodeString(s); err != nil {
		return "", fmt.Errorf("transport: certificate fingerprint is not hex")
	}
	return s, nil
}

// PinnedTLSConfig returns a client TLS configuration that accepts exactly the
// leaf certificate with the given SHA-256 fingerprint, regardless of CA,
// hostname, or validity period (the pin is the trust anchor; it is obtained
// out of band through enrollment or pairing).
func PinnedTLSConfig(fingerprintHex string) (*tls.Config, error) {
	want, err := NormalizeFingerprint(fingerprintHex)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// Chain/hostname verification is replaced by the exact-leaf pin below.
		InsecureSkipVerify: true, //nolint:gosec // pinned via VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("transport: hub presented no certificate")
			}
			got := CertFingerprint(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				return fmt.Errorf("transport: hub certificate fingerprint %s does not match pinned %s (wrong hub, or the hub certificate was replaced: re-enroll/re-pair to update the pin)", got, want)
			}
			return nil
		},
	}, nil
}

// CATLSConfig returns a client configuration with normal CA and hostname
// validation. Nil roots uses the system pool.
func CATLSConfig(roots *x509.CertPool) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
}

// ClientTLSConfig returns PinnedTLSConfig(pin) when pin is non-empty and
// CA-validated TLS with the system roots otherwise.
func ClientTLSConfig(pin string) (*tls.Config, error) {
	if strings.TrimSpace(pin) == "" {
		return CATLSConfig(nil), nil
	}
	return PinnedTLSConfig(pin)
}

// WriteFileAtomic writes data to path via a same-directory temp file with
// the given mode, fsyncs, and renames into place.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if err := fileperm.RestrictFile(f, mode); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}
