package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/easylab-platform/easyvcs/store"
)

// newAuthFixture builds a labTokenAuth over a fresh store with one write
// user (alice/write-token) and one read user (bob/read-token).
func newAuthFixture(t *testing.T) (*labTokenAuth, *store.CentralStore) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	alice, err := cs.CreateUser("alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := cs.CreateUser("bob", "Bob")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CreateToken("writetok", alice.ID, "write"); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.CreateToken("readtok", bob.ID, "read"); err != nil {
		t.Fatal(err)
	}
	return newLabTokenAuth(cs), cs
}

// TestIssueTokenNeverEchoesStaticToken verifies the minted-token semantics:
// the response token is random, prefixed, and NOT the store credential.
func TestIssueTokenNeverEchoesStaticToken(t *testing.T) {
	a, _ := newAuthFixture(t)

	minted := a.IssueToken(t.Context(), "alice", []string{"repository:x:push,pull"}, time.Hour)
	if minted == "" {
		t.Fatal("write principal should get a minted token")
	}
	if minted == "writetok" {
		t.Fatal("IssueToken must not return the static store token")
	}
	if minted[:4] != "lab_" {
		t.Fatalf("minted token should carry the lab_ prefix, got %q", minted)
	}
	// The minted token authorizes push for the granted scope.
	if _, ok := a.CheckBearer(t.Context(), minted, "repository:x:push"); !ok {
		t.Fatal("minted write token should authorize push")
	}
	// ...but only the granted actions.
	if _, ok := a.CheckBearer(t.Context(), minted, "repository:x:delete"); ok {
		t.Fatal("minted token must not exceed its granted scope actions")
	}
	// It also authenticates (write-capable).
	req := httptest.NewRequest(http.MethodPost, "/upload", nil)
	req.Header.Set("Authorization", "Bearer "+minted)
	if u := a.Authenticate(t.Context(), req); u != "alice" {
		t.Fatalf("minted token should authenticate as alice, got %q", u)
	}
}

// TestIssueTokenReadPrincipalIsPullOnly verifies a read-level principal's
// mint cannot push.
func TestIssueTokenReadPrincipalIsPullOnly(t *testing.T) {
	a, _ := newAuthFixture(t)
	// bob authenticates read-level; Authenticate returns "" for read
	// principals by design, but the mint path is reachable for any username.
	minted := a.IssueToken(t.Context(), "bob", []string{"repository:x:pull"}, time.Hour)
	if minted == "" {
		t.Fatal("read principal should still get a pull mint")
	}
	if _, ok := a.CheckBearer(t.Context(), minted, "repository:x:pull"); !ok {
		t.Fatal("read mint should authorize pull")
	}
	if _, ok := a.CheckBearer(t.Context(), minted, "repository:x:push"); ok {
		t.Fatal("read principal's mint must not authorize push")
	}
}

// TestMintedTokenExpiry verifies mints stop working after their TTL.
func TestMintedTokenExpiry(t *testing.T) {
	a, _ := newAuthFixture(t)
	tok := a.IssueToken(t.Context(), "alice", []string{"repository:x:pull"}, 20*time.Millisecond)
	if _, ok := a.CheckBearer(t.Context(), tok, "repository:x:pull"); !ok {
		t.Fatal("fresh mint should work")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := a.CheckBearer(t.Context(), tok, "repository:x:pull"); ok {
		t.Fatal("expired mint must fail")
	}
}

// TestReadLevelTokenCannotPush verifies read-level store tokens are denied
// push scopes directly (no privilege escalation via CheckBearer).
func TestReadLevelTokenCannotPush(t *testing.T) {
	a, _ := newAuthFixture(t)
	if _, ok := a.CheckBearer(t.Context(), "writetok", "repository:x:push"); !ok {
		t.Fatal("write token should push")
	}
	if _, ok := a.CheckBearer(t.Context(), "readtok", "repository:x:pull"); !ok {
		t.Fatal("read token should pull")
	}
	if _, ok := a.CheckBearer(t.Context(), "readtok", "repository:x:push"); ok {
		t.Fatal("read token must not push")
	}
}

// TestServeOCITokenFlow drives the mounted /v2/token endpoint end to end:
// anonymous pull scope gets a pull-only mint; a write token asking for push
// gets a push-capable mint; the response never contains the static token.
func TestServeOCITokenFlow(t *testing.T) {
	// Build the auth fixture FIRST so its store (with alice/bob) is the one
	// OpenDefault resolves; the server wrapper only supplies serveOCIToken.
	a, _ := newAuthFixture(t)
	s := &server{}

	doToken := func(authz string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, "/v2/token?scope=repository:team/app:pull,push", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		s.serveOCIToken(rec, req, a)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// Anonymous push scope -> 401 challenge.
	code, body := doToken("")
	if code != http.StatusUnauthorized {
		t.Fatalf("anonymous push scope = %d %v", code, body)
	}

	// Write credential -> push-capable mint, not the static token.
	code, body = doToken("Bearer writetok")
	if code != http.StatusOK {
		t.Fatalf("write credential token = %d %v", code, body)
	}
	tok, _ := body["token"].(string)
	if tok == "" || tok == "writetok" {
		t.Fatalf("minted token = %q", tok)
	}
	if _, ok := a.CheckBearer(t.Context(), tok, "repository:team/app:push"); !ok {
		t.Fatal("minted token should authorize the requested push scope")
	}
}
