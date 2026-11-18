// Package identity holds what makes this device recognisable to others: for
// now its TLS certificate, whose SHA-256 is its LocalSend fingerprint.
package identity

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Identity is this device's certificate and the fingerprint derived from it.
type Identity struct {
	Cert        tls.Certificate
	Fingerprint string
}

// LoadOrCreate reads the identity saved in dir, creating it on first use.
func LoadOrCreate(dir string) (*Identity, error) {
	certPath, keyPath := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if errors.Is(err, fs.ErrNotExist) {
		if err := create(dir, certPath, keyPath); err != nil {
			return nil, fmt.Errorf("create identity: %w", err)
		}
		cert, err = tls.LoadX509KeyPair(certPath, keyPath)
	}
	if err != nil {
		return nil, fmt.Errorf("load identity: %w", err)
	}
	return &Identity{Cert: cert, Fingerprint: Fingerprint(cert.Certificate[0])}, nil
}

// Fingerprint is the SHA-256 of a DER certificate, as LocalSend writes it.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return strings.ToUpper(hex.EncodeToString(sum[:]))
}

// TLSConfig is the server configuration for LocalSend's HTTPS: our
// self-signed certificate, and a client certificate asked for but not
// required or verified, since peers are self-signed too.
func (id *Identity) TLSConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{id.Cert},
		ClientAuth:   tls.RequestClientCert,
		MinVersion:   tls.VersionTLS12,
	}
}

func create(dir, certPath, keyPath string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// RSA 2048, like the stock LocalSend app's own certificate.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Yonder"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
}
