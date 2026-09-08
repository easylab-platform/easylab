package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
	"github.com/easylab-platform/easyvcs/transfer"
)

// helper: create repo and return a server bound (no auth) to it.
func createRepo(t *testing.T, s *server, ns, name string) *store.Repo {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/repositories/"+ns+"/"+name, nil)
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create repo: %d %s", rec.Code, rec.Body.String())
	}
	repo, err := s.cs.OpenRepo(store.RepoRef{Namespace: ns, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return repo
}

func TestRepoLifecycleViaAPI(t *testing.T) {
	s := newTestServer(t)
	repo := createRepo(t, s, "team", "app")

	// Create duplicate -> 409
	req := httptest.NewRequest(http.MethodPost, "/repositories/team/app", nil)
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate: expected 409, got %d", rec.Code)
	}

	// List
	req = httptest.NewRequest(http.MethodGet, "/repositories", nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d", rec.Code)
	}
	var repos []store.RepoRef
	_ = json.Unmarshal(rec.Body.Bytes(), &repos)
	if len(repos) != 1 || repos[0].String() != "team/app" {
		t.Fatalf("repos: %+v", repos)
	}

	// Delete missing -> 404
	req = httptest.NewRequest(http.MethodDelete, "/repositories/team/nope", nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing: %d", rec.Code)
	}

	// Delete existing
	req = httptest.NewRequest(http.MethodDelete, "/repositories/team/app", nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: %d", rec.Code)
	}
	_ = repo
}

func TestCommitFromChangesViaAPI(t *testing.T) {
	s := newTestServer(t)
	createRepo(t, s, "team", "app")
	// Root commit with changes. Empty parent_hash -> root, so skip HexToID.
	body, _ := json.Marshal(map[string]any{
		"parent_hash": "",
		"changes": []map[string]any{
			{"path": "a.txt", "content": "hello\n"},
		},
		"description": "root",
	})
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/commit", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("commit: %d %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["revision_id"] == "" || resp["snapshot"] == "" {
		t.Fatalf("missing ids: %v", resp)
	}
}

func TestCommitBadParent(t *testing.T) {
	s := newTestServer(t)
	createRepo(t, s, "team", "app")
	body, _ := json.Marshal(map[string]any{
		"parent_hash": "0000000000000000000000000000000000000000000000000000000000000001",
		"changes":     []map[string]any{{"path": "a", "content": "x"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/commit", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad parent, got %d", rec.Code)
	}
}

func TestCommitBadHex(t *testing.T) {
	s := newTestServer(t)
	createRepo(t, s, "team", "app")
	body, _ := json.Marshal(map[string]any{
		"parent_hash": "NOTHEX",
		"changes":     []map[string]any{{"path": "a", "content": "x"}},
	})
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/commit", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid hex, got %d", rec.Code)
	}
}

func TestCommitTreeModeInvalid(t *testing.T) {
	s := newTestServer(t)
	createRepo(t, s, "team", "app")
	// No changes, no tree_id.
	body, _ := json.Marshal(map[string]any{"description": "x"})
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/commit", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing tree_id, got %d", rec.Code)
	}

	// Bad tree_id hex
	body, _ = json.Marshal(map[string]any{"tree_id": "bad", "description": "x"})
	req = httptest.NewRequest(http.MethodPost, "/repo/team/app/commit", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad tree_id, got %d", rec.Code)
	}
}

func TestRebaseSquashResolveViaAPI(t *testing.T) {
	s := newTestServer(t)
	repo := createRepo(t, s, "team", "app")
	ws := revision.NewWorkspace(repo)
	// seed a commit
	snap, ch, _ := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "f.txt", Content: []byte("a")}}, "c1", store.Author{Name: "n"}, "")

	// Rebase onto itself
	body, _ := json.Marshal(map[string]any{"revision_id": ch.ID, "new_parents": []string{snap.RevisionHash.String()}})
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/rebase", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("rebase: %d %s", rec.Code, rec.Body.String())
	}

	// Rebasing the root commit onto itself now gives it a parent, so squash
	// should succeed.
	body, _ = json.Marshal(map[string]any{"revision_id": ch.ID})
	req = httptest.NewRequest(http.MethodPost, "/repo/team/app/squash", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("squash after rebase should succeed, got %d %s", rec.Code, rec.Body.String())
	}

	// Merge
	body, _ = json.Marshal(map[string]any{"base": snap.TreeID.String(), "ours": snap.TreeID.String(), "theirs": snap.TreeID.String()})
	req = httptest.NewRequest(http.MethodPost, "/repo/team/app/merge", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("merge: %d %s", rec.Code, rec.Body.String())
	}
}

func TestRefsViaAPI(t *testing.T) {
	s := newTestServer(t)
	repo := createRepo(t, s, "team", "app")
	ws := revision.NewWorkspace(repo)
	_, ch, _ := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("a")}}, "c", store.Author{Name: "n"}, "")

	// set ref
	body, _ := json.Marshal(map[string]any{"kind": "branch", "target": ch.ID})
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/ref/main", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setref: %d", rec.Code)
	}
	// list refs
	req = httptest.NewRequest(http.MethodGet, "/repo/team/app/refs", nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("listrefs: %d", rec.Code)
	}
	var refs []store.Ref
	_ = json.Unmarshal(rec.Body.Bytes(), &refs)
	if len(refs) != 1 {
		t.Fatalf("refs: %d", len(refs))
	}
	// delete ref
	req = httptest.NewRequest(http.MethodDelete, "/repo/team/app/ref/main", nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deleteref: %d", rec.Code)
	}
	// delete missing -> 404
	req = httptest.NewRequest(http.MethodDelete, "/repo/team/app/ref/nope", nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing ref: %d", rec.Code)
	}
}

func TestLogChangeDiffViaAPI(t *testing.T) {
	s := newTestServer(t)
	repo := createRepo(t, s, "team", "app")
	ws := revision.NewWorkspace(repo)
	tree := object.NewTree()
	tree.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: object.BlobID([]byte("x"))}
	snap, ch, _ := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "f.txt", Content: []byte("x")}}, "c", store.Author{Name: "n"}, "")

	// log
	req := httptest.NewRequest(http.MethodGet, "/repo/team/app/log", nil)
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("log: %d", rec.Code)
	}
	var entries []map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &entries)
	if len(entries) != 1 {
		t.Fatalf("log entries: %d", len(entries))
	}

	// change (revision) detail
	req = httptest.NewRequest(http.MethodGet, "/repo/team/app/revision/"+ch.ID, nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("revision: %d", rec.Code)
	}
	// missing revision -> 404
	req = httptest.NewRequest(http.MethodGet, "/repo/team/app/revision/nope", nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing revision: %d", rec.Code)
	}

	// diff
	req = httptest.NewRequest(http.MethodGet, "/repo/team/app/diff/"+snap.RevisionHash.String()+"/"+snap.RevisionHash.String(), nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("diff: %d", rec.Code)
	}
	// bad diff hex
	req = httptest.NewRequest(http.MethodGet, "/repo/team/app/diff/bad/bad", nil)
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad diff: %d", rec.Code)
	}
}

func TestSmartProtocolPushFetchAdvertise(t *testing.T) {
	s := newTestServer(t)
	repo := createRepo(t, s, "team", "app")
	ws := revision.NewWorkspace(repo)
	_, ch, err := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{{Path: "a.txt", Content: []byte("a")}}, "c", store.Author{Name: "n"}, "")
	if err != nil {
		t.Fatal(err)
	}

	// advertise
	req := httptest.NewRequest(http.MethodPost, "/repo/team/app/advertise", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("advertise: %d", rec.Code)
	}
	var adv advertiseResp
	_ = json.Unmarshal(rec.Body.Bytes(), &adv)
	if len(adv.Changes) != 1 {
		t.Fatalf("advertise changes: %d", len(adv.Changes))
	}

	// fetch
	req = httptest.NewRequest(http.MethodPost, "/repo/team/app/fetch", bytes.NewReader([]byte(`{"have":["c"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fetch: %d", rec.Code)
	}
	// push (fetch body gzip not needed; plain bundle)
	b, _ := transfer.CollectAll(repo)
	req = httptest.NewRequest(http.MethodPost, "/repo/team/app/push", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	_ = b
	_ = ch
	// push with empty body -> decodeBundleBody tries binary then json; {} is json -> apply 0.
	if rec.Code != http.StatusOK {
		t.Fatalf("push: %d", rec.Code)
	}
}

func TestSnapshotOrTreeHelper(t *testing.T) {
	s := newTestServer(t)
	repo := createRepo(t, s, "team", "app")
	ws := revision.NewWorkspace(repo)
	tree := object.NewTree()
	tree.Entries["f"] = object.Entry{Name: "f", Kind: object.KindBlob, ID: object.BlobID([]byte("x"))}
	tid := snapshotOrTree(ws, tree.ID())
	if tid != tree.ID() {
		t.Fatalf("snapshotOrTree on tree id should passthrough")
	}
}
