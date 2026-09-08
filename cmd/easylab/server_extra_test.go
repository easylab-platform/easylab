package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
	"github.com/easylab-platform/easyvcs/transfer"
)

func TestAuthOKOpenInstance(t *testing.T) {
	// A server whose store has no users is an open instance: anonymous OK.
	s := newLabServer(t)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if !s.authOK(r) {
		t.Fatal("open instance => authOK should be true")
	}
}

func TestAuthOKRequiresStoreToken(t *testing.T) {
	s := newLabServer(t)
	seedTestToken(t, s, "storetok")
	// Anonymous is denied (instance closed).
	if s.authOK(httptest.NewRequest(http.MethodGet, "/", nil)) {
		t.Fatal("closed instance should deny anonymous")
	}
	// A registered token passes; an unknown one fails.
	ok := httptest.NewRequest(http.MethodGet, "/", nil)
	ok.Header.Set("Authorization", "Bearer storetok")
	if !s.authOK(ok) {
		t.Fatal("registered token should pass")
	}
	bad := httptest.NewRequest(http.MethodGet, "/", nil)
	bad.Header.Set("Authorization", "Bearer wrong")
	if s.authOK(bad) {
		t.Fatal("unknown token must fail")
	}
}

func TestAuthOKBearer(t *testing.T) {
	s := newLabServer(t)
	seedTestToken(t, s, "secret")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer secret")
	if !s.authOK(r) {
		t.Fatal("valid token should pass")
	}
	r2 := httptest.NewRequest(http.MethodGet, "/", nil)
	r2.Header.Set("Authorization", "Bearer wrong")
	if s.authOK(r2) {
		t.Fatal("wrong token should fail")
	}
	r3 := httptest.NewRequest(http.MethodGet, "/", nil)
	if s.authOK(r3) {
		t.Fatal("missing header should fail")
	}
	r4 := httptest.NewRequest(http.MethodGet, "/", nil)
	r4.Header.Set("Authorization", "Basic xyz")
	if s.authOK(r4) {
		t.Fatal("non-bearer should fail")
	}
}

func TestChangeSetWithAncestors(t *testing.T) {
	s := newTestServer(t)
	repo := createRepo(t, s, "team", "app")
	ws := revision.NewWorkspace(repo)
	snap1, ch1, _ := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("a")}}, "c1", store.Author{Name: "n"}, "")
	_, ch2, _ := ws.CommitFromChanges(snap1.RevisionHash, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("b")}}, "c2", store.Author{Name: "n"}, "")

	res := changeSetWithAncestors(repo, []string{ch2.ID}, map[string]bool{})
	if !res[ch1.ID] || !res[ch2.ID] {
		t.Fatalf("ancestors should include ch1 and ch2: %v", res)
	}
	// have prefilled skips
	res2 := changeSetWithAncestors(repo, []string{ch2.ID}, map[string]bool{ch1.ID: true})
	if res2[ch1.ID] {
		t.Fatal("ch1 should be skipped when in have")
	}
	if !res2[ch2.ID] {
		t.Fatal("ch2 should still be included")
	}
}

func TestFilterBundle(t *testing.T) {
	s := newTestServer(t)
	repo := createRepo(t, s, "team", "app")
	b, _ := transfer.CollectAll(repo)
	// Empty wanted returns bundle unchanged.
	out := filterBundle(b, nil)
	if out != b {
		t.Fatal("nil wanted should return same bundle")
	}
	// Filter for a wanted revision id (none -> empty changes).
	out2 := filterBundle(b, map[string]bool{"nope": true})
	if len(out2.Revisions) != 0 {
		t.Fatalf("filter to unknown should drop all changes, got %d", len(out2.Revisions))
	}
}

func TestRepoRefHelper(t *testing.T) {
	s := newTestServer(t)
	_, ok := repoRef(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	_ = s
	if ok {
		t.Fatal("repoRef helper should return false")
	}
}

func TestHandlePushEmptyBody(t *testing.T) {
	s := newTestServer(t)
	createRepo(t, s, "team", "app")
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("push empty json: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHandleFetchBadJSON(t *testing.T) {
	s := newTestServer(t)
	createRepo(t, s, "team", "app")
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/fetch", bytes.NewReader([]byte("not json")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("fetch bad json: %d", rec.Code)
	}
}
