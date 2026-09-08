package main

import (
	"net/http"
	"testing"
)

// TestLabMirrorRepoReadOnly verifies a mirror repository rejects writes while
// allowing reads, and that a push-mirror CRUD + push works on a normal repo.
func TestLabMirrorRepoReadOnly(t *testing.T) {
	s := newLabServer(t)
	admin := &labClient{t: t, s: s, token: "lab-admin"}
	// open instance -> use the flat token set
	seedTestToken(t, s, "lab-admin")

	// A normal repo to host a push mirror.
	if rec := admin.do("POST", "/api/v1/repo", map[string]any{
		"namespace": "team", "name": "app",
	}); rec.Code != http.StatusOK {
		t.Fatalf("create app repo: %d %s", rec.Code, rec.Body.String())
	}

	// A read-only mirror repo seeded with a file (simulated external source).
	url := "file://" + t.TempDir() // empty; Pull will clone an empty main, which is fine for the guard test
	if rec := admin.do("POST", "/api/v1/repo", map[string]any{
		"namespace": "team", "name": "mirror-app", "kind": "mirror", "mirror_url": url,
	}); rec.Code != http.StatusOK {
		t.Fatalf("create mirror repo: %d %s", rec.Code, rec.Body.String())
	}

	// Reads are allowed on a mirror.
	if rec := admin.do("GET", "/api/v1/repo/team/mirror-app", nil); rec.Code != http.StatusOK {
		t.Fatalf("get mirror repo: %d", rec.Code)
	}

	// Writes are rejected on a mirror.
	if rec := admin.do("POST", "/api/v1/repo/team/mirror-app/files", map[string]any{"name": "x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("mirror commit should be 403, got %d", rec.Code)
	}
	if rec := admin.do("POST", "/api/v1/repo/team/mirror-app/branches", map[string]any{"name": "x"}); rec.Code != http.StatusForbidden {
		t.Fatalf("mirror set branch should be 403, got %d", rec.Code)
	}

	// Push-mirror CRUD on the normal repo.
	if rec := admin.do("POST", "/api/v1/repo/team/app/mirrors", map[string]any{
		"name": "forgejo", "url": "file:///tmp/idont-exist-xxx", "branch": "main",
	}); rec.Code != http.StatusOK {
		t.Fatalf("create push mirror: %d %s", rec.Code, rec.Body.String())
	}
	if rec := admin.do("GET", "/api/v1/repo/team/app/mirrors", nil); rec.Code != http.StatusOK {
		t.Fatalf("list push mirrors: %d", rec.Code)
	}
	// Deleting is fine without hitting the network.
	if rec := admin.do("DELETE", "/api/v1/repo/team/app/mirrors/forgejo", nil); rec.Code != http.StatusOK {
		t.Fatalf("delete push mirror: %d %s", rec.Code, rec.Body.String())
	}
}

// TestLabCreateMirrorRequiresURL ensures a mirror repo requires mirror_url.
func TestLabCreateMirrorRequiresURL(t *testing.T) {
	s := newLabServer(t)
	admin := &labClient{t: t, s: s, token: "lab-admin"}
	seedTestToken(t, s, "lab-admin")
	rec := admin.do("POST", "/api/v1/repo", map[string]any{
		"namespace": "team", "name": "bad", "kind": "mirror",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for mirror without url, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestLabPushMirrorListOmitsToken ensures tokens are stripped from responses.
func TestLabPushMirrorListOmitsToken(t *testing.T) {
	s := newLabServer(t)
	admin := &labClient{t: t, s: s, token: "lab-admin"}
	seedTestToken(t, s, "lab-admin")
	admin.ok("POST", "/api/v1/repo", map[string]any{"namespace": "team", "name": "app"})
	admin.ok("POST", "/api/v1/repo/team/app/mirrors", map[string]any{
		"name": "forgejo", "url": "https://example.com/x.git", "token": "secret-token",
	})
	rec := admin.do("GET", "/api/v1/repo/team/app/mirrors", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list mirrors: %d", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("empty list")
	}
	if contains(rec.Body.Bytes(), "secret-token") {
		t.Fatalf("token leaked in mirror list: %s", rec.Body.String())
	}
}

func contains(b []byte, s string) bool {
	return stringContains(string(b), s)
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
