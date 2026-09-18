package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// ensureEgressCA returns the per-deployment egress MITM CA (cert PEM, key
// PEM), generating and persisting it under dir on first use. The CA is the
// trust root every easysidecar-injected workload gets (SSL_CERT_FILE etc.);
// losing it only invalidates future leaf certificates, so persistence is
// best-effort (a regenerated CA is announced in the log).
func ensureEgressCA(dir string) (string, string, error) {
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	if cert, key, err := readEgressCA(certPath, keyPath); err == nil {
		return cert, key, nil
	}
	// Generate a fresh CA.
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return "", "", fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "easylab-egress-ca", Organization: []string{"easylab"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		MaxPathLen:            0,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return "", "", err
	}
	return string(certPEM), string(keyPEM), nil
}

// readEgressCA loads a persisted CA pair.
func readEgressCA(certPath, keyPath string) (string, string, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return "", "", err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return "", "", err
	}
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return "", "", fmt.Errorf("invalid CA PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return "", "", err
	}
	if _, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes); err != nil {
		return "", "", err
	}
	if time.Now().After(cert.NotAfter) {
		return "", "", fmt.Errorf("CA expired")
	}
	return string(certPEM), string(keyPEM), nil
}
