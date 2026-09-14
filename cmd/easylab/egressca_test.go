package main

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureEgressCA verifies generation, persistence (second call returns
// the SAME CA), and reload validity.
func TestEnsureEgressCA(t *testing.T) {
	dir := t.TempDir()
	cert1, key1, err := ensureEgressCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Files exist with the right perms.
	fi, err := os.Stat(filepath.Join(dir, "ca.key"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key perm = %v", fi.Mode().Perm())
	}
	// Second call is stable.
	cert2, key2, err := ensureEgressCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	if cert1 != cert2 || key1 != key2 {
		t.Fatal("CA must be stable across calls")
	}
	// The certificate is a parseable CA.
	block, _ := pem.Decode([]byte(cert1))
	if block == nil {
		t.Fatal("bad cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !cert.IsCA {
		t.Fatal("generated certificate is not a CA")
	}
	// Corrupted dir regenerates.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ensureEgressCA(dir); err != nil {
		t.Fatalf("regeneration after removal: %v", err)
	}
}
