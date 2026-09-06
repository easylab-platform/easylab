package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"easyvcs/internal/object"
	"easyvcs/internal/revision"
	"easyvcs/internal/store"
)

// TestSquashRootViaAPI ensures a root revision (no parents) squashing returns 400.
func TestSquashRootViaAPI(t *testing.T) {
	s := newTestServer(t)
	repo := createRepo(t, s, "team", "app")
	ws := revision.NewWorkspace(repo)
	_, ch, _ := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("a")}}, "c", store.Author{Name: "n"}, "")
	body, _ := json.Marshal(map[string]any{"revision_id": ch.ID})
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/squash", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("squash root: expected 400, got %d", rec.Code)
	}
}

func TestRebaseBadHexAndMissing(t *testing.T) {
	s := newTestServer(t)
	repo := createRepo(t, s, "team", "app")
	ws := revision.NewWorkspace(repo)
	_, ch, _ := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("a")}}, "c", store.Author{Name: "n"}, "")
	// bad hex
	body, _ := json.Marshal(map[string]any{"revision_id": ch.ID, "new_parents": []string{"bad"}})
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/rebase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rebase bad hex: %d", rec.Code)
	}
	// missing revision
	body, _ = json.Marshal(map[string]any{"revision_id": "nope", "new_parents": []string{"0000000000000000000000000000000000000000000000000000000000000000"}})
	req = httptest.NewRequest(http.MethodPost, "/repo/team/app/rebase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rebase missing: %d", rec.Code)
	}
}

func TestSetRefInvalidKind(t *testing.T) {
	s := newTestServer(t)
	repo := createRepo(t, s, "team", "app")
	ws := revision.NewWorkspace(repo)
	_, ch, _ := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("a")}}, "c", store.Author{Name: "n"}, "")
	// invalid kind defaults to branch
	body, _ := json.Marshal(map[string]any{"kind": "weird", "target": ch.ID})
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/ref/main", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setref invalid kind: %d", rec.Code)
	}
	refs, _ := ws.ListRefs()
	if len(refs) != 1 || refs[0].Kind != store.RefBranch {
		t.Fatalf("refs: %+v", refs)
	}
}

func TestGzipBodyDecode(t *testing.T) {
	s := newTestServer(t)
	repo := createRepo(t, s, "team", "app")
	ws := revision.NewWorkspace(repo)
	_, ch, _ := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("a")}}, "c", store.Author{Name: "n"}, "")
	// gzip a JSON commit request and ensure decodeJSONBody transparently handles it.
	reqBody, _ := json.Marshal(map[string]any{"description": "gzip", "parent_hash": "", "changes": []map[string]any{{"path": "b.txt", "content": "b"}}})
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write(reqBody)
	_ = gw.Close()
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/commit", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("gzip commit: %d %s", rec.Code, rec.Body.String())
	}
	_ = ch
}
