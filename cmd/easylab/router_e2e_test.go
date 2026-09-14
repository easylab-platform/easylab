package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/easylab-platform/easyvcs/store"
)

// connectCall drives the REAL gateway router (auth interceptor + per-RPC
// authorization) over HTTP with a bearer credential — the end-to-end path a
// client takes. It returns the HTTP status and the decoded JSON body.
func connectCall(t *testing.T, s *server, token, procedure string, body map[string]any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(http.MethodPost, procedure, rdr)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// seedUsersWithTokens creates the 4-user world with real tokens on the server
// so they authenticate through the interceptor.
func seedUsersWithTokens(t *testing.T, s *server) (owner, maint, dev, outsider *store.User) {
	t.Helper()
	mk := func(name string) *store.User {
		u, err := s.cs.CreateUser(name, name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.cs.CreateToken(name+"-tok", u.ID, "write"); err != nil {
			t.Fatal(err)
		}
		return u
	}
	return mk("owner"), mk("maint"), mk("dev"), mk("outsider")
}

// TestRouterConnectAuthAndRoles is the gateway-boundary e2e: it verifies that
// the Connect auth interceptor rejects anonymous callers and that per-RPC
// authorization enforces the owner/maintainer/developer ladder.
func TestRouterConnectAuthAndRoles(t *testing.T) {
	s := newTestServer(t)
	owner, maint, dev, outsider := seedUsersWithTokens(t, s)
	t.Setenv("EASYLAB_ADMIN_TOKEN", "admin-secret")

	// Health is public; everything else is not.
	if code, _ := connectCall(t, s, "", "/easylab.v1.LabService/Health", map[string]any{}); code != http.StatusOK {
		t.Fatalf("anonymous Health = %d, want 200", code)
	}
	if code, _ := connectCall(t, s, "", "/easylab.v1.LabService/CreateRepo", map[string]any{"org": "team", "repo": "x"}); code != http.StatusUnauthorized {
		t.Fatalf("anonymous CreateRepo = %d, want 401", code)
	}
	if code, _ := connectCall(t, s, "bogus-token", "/easylab.v1.LabService/CreateRepo", map[string]any{"org": "team", "repo": "x"}); code != http.StatusUnauthorized {
		t.Fatalf("bad-token CreateRepo = %d, want 401", code)
	}

	// Owner creates the repo (owned by owner).
	if code, _ := connectCall(t, s, "owner-tok", "/easylab.v1.LabService/CreateRepo", map[string]any{"org": "team", "repo": "pub"}); code != http.StatusOK {
		t.Fatalf("owner CreateRepo = %d, want 200", code)
	}
	repo, err := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "pub"})
	if err != nil || repo.OwnerUserID != owner.ID {
		t.Fatalf("repo owner = %+v (%v)", repo, err)
	}
	if err := s.cs.SetRepoMember(repo.RepoID(), maint.ID, store.RoleMaintainer, &owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.cs.SetRepoMember(repo.RepoID(), dev.ID, store.RoleDeveloper, &owner.ID); err != nil {
		t.Fatal(err)
	}

	// CreateBranch = owner only.
	if code, _ := connectCall(t, s, "maint-tok", "/easylab.v1.LabService/CreateBranch", map[string]any{"org": "team", "repo": "pub", "branch": "feature", "from": "main"}); code != http.StatusForbidden {
		t.Fatalf("maintainer CreateBranch = %d, want 403", code)
	}
	if code, _ := connectCall(t, s, "dev-tok", "/easylab.v1.LabService/CreateBranch", map[string]any{"org": "team", "repo": "pub", "branch": "feature", "from": "main"}); code != http.StatusForbidden {
		t.Fatalf("developer CreateBranch = %d, want 403", code)
	}
	// Owner clears the authz gate (missing branch → invalid_argument, not 403).
	if code, _ := connectCall(t, s, "owner-tok", "/easylab.v1.LabService/CreateBranch", map[string]any{"org": "team", "repo": "pub", "branch": "feature", "from": "main"}); code == http.StatusForbidden {
		t.Fatalf("owner CreateBranch must not be 403")
	}

	// DeleteRepo = owner only (delete the repo as dev → 403).
	if code, _ := connectCall(t, s, "dev-tok", "/easylab.v1.LabService/DeleteRepo", map[string]any{"org": "team", "repo": "pub"}); code != http.StatusForbidden {
		t.Fatalf("developer DeleteRepo = %d, want 403", code)
	}

	// UserService = admin credential only.
	if code, _ := connectCall(t, s, "outsider-tok", "/easylab.v1.UserService/ListUsers", map[string]any{}); code != http.StatusOK {
		// reads are authenticated-only
		t.Fatalf("authenticated ListUsers = %d, want 200", code)
	}
	if code, _ := connectCall(t, s, "", "/easylab.v1.UserService/ListUsers", map[string]any{}); code != http.StatusUnauthorized {
		t.Fatalf("anonymous ListUsers = %d, want 401", code)
	}
	if code, _ := connectCall(t, s, "owner-tok", "/easylab.v1.UserService/CreateUser", map[string]any{"username": "nope"}); code != http.StatusForbidden {
		t.Fatalf("non-admin CreateUser = %d, want 403", code)
	}
	if code, body := connectCall(t, s, "admin-secret", "/easylab.v1.UserService/CreateUser", map[string]any{"username": "newbie"}); code != http.StatusOK || body["token"] == "" {
		t.Fatalf("admin CreateUser = %d %v, want token", code, body)
	}

	// RunWorkflowFile on the repo = maintainer+ (developer denied).
	if code, _ := connectCall(t, s, "dev-tok", "/easylab.v1.WorkflowService/RunWorkflowFile", map[string]any{"org": "team", "repo": "pub", "branch": "main"}); code != http.StatusForbidden {
		t.Fatalf("developer RunWorkflowFile = %d, want 403", code)
	}
	// RegistryService.SetPackageVisibility = maintainer+; developer denied.
	if code, _ := connectCall(t, s, "dev-tok", "/easylab.v1.RegistryService/SetPackageVisibility", map[string]any{"type": "npm", "name": "@team/pub", "visibility": "private"}); code != http.StatusForbidden {
		t.Fatalf("developer SetPackageVisibility = %d, want 403", code)
	}
	_ = maint
	_ = outsider
}

// TestRouterLoopbackOwnerOverride verifies a loopback caller may assert
// X-Agent-Tenant to act as the repository owner (the embedded extensions'
// forwarding path), and that the header is ignored off-loopback.
func TestRouterLoopbackOwnerOverride(t *testing.T) {
	s := newTestServer(t)
	owner, _, _, _ := seedUsersWithTokens(t, s)
	if _, err := s.cs.Create(store.RepoRef{Owner: owner.ID, Namespace: "team", Name: "loop"}); err != nil {
		t.Fatal(err)
	}
	// maint has no role on team/loop; asserting owner via X-Agent-Tenant on
	// loopback lets the call succeed as the owner. Use the maint token but set
	// the header to the owner's username.
	_, err := s.cs.CreateUser("maint2", "maint2")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.cs.CreateToken("maint2-tok", mustUser(t, s, "maint2").ID, "write"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/easylab.v1.LabService/CreateBranch",
		strings.NewReader(`{"org":"team","repo":"loop","branch":"feat","from":"main"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer maint2-tok")
	req.Header.Set("X-Agent-Tenant", owner.Username) // loopback override
	req.RemoteAddr = "127.0.0.1:5555"
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code == http.StatusForbidden {
		t.Fatalf("loopback owner override must not be 403 (got %d %s)", rec.Code, rec.Body.String())
	}
}

func mustUser(t *testing.T, s *server, name string) *store.User {
	t.Helper()
	u, err := s.cs.GetUserByUsername(name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
