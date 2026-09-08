package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	artifactkit "github.com/easylab-platform/artifact/core"
	"github.com/easylab-platform/easyvcs/store"
)

func newTestServer(t *testing.T) *server {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
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
	return &server{cs: cs, registry: reg, ops: opsState, auth: artifactkit.NewStoreAuth(newEasyvcsTokenStore(cs))}
}

// newAuthServer returns a server with auth enabled for a single token.
func newAuthServer(t *testing.T) *server {
	t.Helper()
	s := newTestServer(t)
	seedTestToken(t, s, "secret-token")
	return s
}

func TestAuthRequiredForWrite(t *testing.T) {
	s := newAuthServer(t)
	// Create repo requires auth.
	req := httptest.NewRequest(http.MethodPost, "/repositories/n/r1", nil)
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("create repo without token: expected 401, got %d", rec.Code)
	}

	// With token, succeeds.
	req = httptest.NewRequest(http.MethodPost, "/repositories/n/r1", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create repo with token: expected 200, got %d %s", rec.Code, rec.Body.String())
	}

	// Push requires auth.
	req = httptest.NewRequest(http.MethodPost, "/repo/n/r1/push", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("push without token: expected 401, got %d", rec.Code)
	}
}

func TestReadEndpointsOpenWithoutAuth(t *testing.T) {
	s := newAuthServer(t)
	// Create repo with token first.
	req := httptest.NewRequest(http.MethodPost, "/repositories/n/r1", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d", rec.Code)
	}
	// Log (read) is open without token.
	req = httptest.NewRequest(http.MethodGet, "/repo/n/r1/log", nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("log without token: expected 200, got %d", rec.Code)
	}
}

// TestCentralObjectDedupAndRepoIsolation verifies cross-repo object
// deduplication and per-repo change isolation using a server with two repos.
func TestCentralObjectDedupAndRepoIsolation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	s := &server{cs: cs}

	// Create two repos via the API.
	req := httptest.NewRequest(http.MethodPost, "/repositories/n/r1", nil)
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create r1: %d %s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest(http.MethodPost, "/repositories/n/r2", nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create r2: %d %s", rec.Code, rec.Body.String())
	}

	r1, _ := cs.OpenRepo(store.RepoRef{Namespace: "n", Name: "r1"})
	r2, _ := cs.OpenRepo(store.RepoRef{Namespace: "n", Name: "r2"})

	// Global object dedup: same blob used in both repos is stored once.
	blob := &object.Object{Kind: object.KindBlob, Blob: []byte("same")}
	_ = r1.WriteObject(blob)
	_ = r2.WriteObject(blob)
	var count int
	cs.QueryCount(&count)
	if count != 1 {
		t.Fatalf("expected dedup to 1, got %d", count)
	}

	// Isolation: change in r1 not visible in r2.
	ws1 := revision.NewWorkspace(r1)
	_, ch1, _ := ws1.Commit(revision.CommitParams{TreeID: object.BlobID([]byte("x")), Description: "a", Author: store.Author{Name: "t"}})
	if _, err := r2.GetRevision(ch1.ID); err == nil {
		t.Fatalf("cross-repo change leak: r2 sees r1 change")
	}

	t.Logf("central dedup+isolation ok: %d objects shared", count)
}

// TestAPICreateAndLog exercises repository creation, commit routing, and log
// through the server handler with a real repo in the central store.
func TestAPICreateAndLog(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/repositories/team/app", nil)
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}

	repo, err := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	// Seed a commit via the workspace to have a log entry.
	ws := revision.NewWorkspace(repo)
	tree := object.NewTree()
	tree.Entries["a.txt"] = object.Entry{Name: "a.txt", Kind: object.KindBlob, ID: object.BlobID([]byte("hi"))}
	ws.WriteTree(tree)
	snap, ch, _ := ws.Commit(revision.CommitParams{TreeID: tree.ID(), Description: "first", Author: store.Author{Name: "t"}})
	_ = snap

	req = httptest.NewRequest(http.MethodGet, "/repo/team/app/log", nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("log: %d %s", rec.Code, rec.Body.String())
	}
	var entries []map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &entries)
	if len(entries) != 1 || entries[0]["revision_id"] != ch.ID {
		t.Fatalf("bad log: %+v", entries)
	}

	// Repo list should contain team/app.
	req = httptest.NewRequest(http.MethodGet, "/repositories", nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var repos []store.RepoRef
	_ = json.Unmarshal(rec.Body.Bytes(), &repos)
	if len(repos) != 1 || repos[0].String() != "team/app" {
		t.Fatalf("bad repos: %+v", repos)
	}

	// Change detail endpoint.
	req = httptest.NewRequest(http.MethodGet, "/repo/team/app/revision/"+ch.ID, nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("change: %d", rec.Code)
	}
	t.Logf("api create+log ok: %s", ch.ID)
}
