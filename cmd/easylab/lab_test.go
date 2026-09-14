package main

import (
	"testing"

	artifactkit "github.com/easylab-platform/artifact/core"
	"github.com/easylab-platform/easylab/internal/sbxreg"
	"github.com/easylab-platform/easyvcs/store"
)

// Shared test fixtures (kept after retiring the legacy /api/v1 REST tests).
// The REST-era labClient helpers and the REST-driven test files were removed
// with the REST surface; RBAC and RPC coverage now lives in rbac_test.go and
// router_e2e_test.go.

func newLabServer(t *testing.T) *server {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.SetWAL(); err != nil {
		t.Fatal(err)
	}
	reg, err := openRegistry(home)
	if err != nil {
		t.Fatal(err)
	}
	opsState, err := newOpsState()
	if err != nil {
		t.Fatal(err)
	}
	sbx, err := sbxreg.Open(store.KindSQLite, store.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	return &server{cs: cs, registry: reg, ops: opsState, sbx: sbx, auth: artifactkit.NewStoreAuth(newEasyvcsTokenStore(cs))}
}

// seedTestToken registers a write-level credential in the server's store so the
// instance is closed (authenticated requests required).
func seedTestToken(t *testing.T, s *server, token string) {
	t.Helper()
	u, err := s.cs.CreateUser("test-admin", "Test Admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.cs.CreateToken(token, u.ID, "write"); err != nil {
		t.Fatal(err)
	}
}
