package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"easyvcs/internal/object"
	"easyvcs/internal/revision"
	"easyvcs/internal/store"
	"easyvcs/internal/transfer"
)

// TestFetchRecordsRemoteRefsVerifies that cmdFetch records all remote branchs
// into the remote_refs namespace (git's refs/remotes) and sets a default one.
func TestFetchRecordsRemoteRefs(t *testing.T) {
	setHome(t)
	remoteRepo := createRepoIn(t, "demo", "source")
	ws := revision.NewWorkspace(remoteRepo)
	// two branchs: main and feature.
	tree := object.NewTree()
	tree.Entries["base.txt"] = object.Entry{Name: "base.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("base"))}
	s1, r1, e1 := ws.Commit(revision.CommitParams{TreeID: ws.WriteTree(tree), Description: "base", Author: store.Author{Name: "t", Email: "t@x"}})
	if e1 != nil {
		t.Fatal(e1)
	}
	tree2 := object.NewTree()
	tree2.Entries["base.txt"] = object.Entry{Name: "base.txt", Kind: object.KindBlob, ID: s1.TreeID}
	tree2.Entries["feat.txt"] = object.Entry{Name: "feat.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("f"))}
	_, r2, e2 := ws.Commit(revision.CommitParams{Parents: []object.ID{s1.RevisionHash}, TreeID: ws.WriteTree(tree2), Description: "feat", Author: store.Author{Name: "t", Email: "t@x"}})
	if e2 != nil {
		t.Fatal(e2)
	}
	if _, err := ws.SetRef("main", store.RefBranch, r1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.SetRef("feature", store.RefBranch, r2.ID); err != nil {
		t.Fatal(err)
	}
	srv := stubRemoteServer2(t, remoteRepo)
	defer srv.Close()

	setHome(t)
	localRepo := createRepoIn(t, "demo", "source")
	if err := localRepo.PutRemote("origin", srv.URL, ""); err != nil {
		t.Fatal(err)
	}

	if err := doFetch(localRepo, []string{"origin"}); err != nil {
		t.Fatalf("doFetch: %v", err)
	}

	refs, err := localRepo.ListRemoteRefs("origin")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) < 2 {
		t.Fatalf("expected >=2 remote refs, got %d", len(refs))
	}
	names := map[string]bool{}
	for _, rr := range refs {
		names[rr.Name] = true
	}
	if !names["main"] || !names["feature"] {
		t.Fatalf("remote refs should include main+feature: %v", names)
	}
	if def, _ := localRepo.GetRemoteDefaultBranch("origin"); def == "" {
		t.Fatalf("remote default branch should be set, got empty")
	}
}

// TestPullDefaultBranch verifies pull with no branch uses the remote default
// and merges the remote tip onto the local (git-mental model).
func TestPullDefaultBranch(t *testing.T) {
	setHome(t)
	remoteRepo := createRepoIn(t, "demo", "source")
	ws := revision.NewWorkspace(remoteRepo)
	tree := object.NewTree()
	tree.Entries["base.txt"] = object.Entry{Name: "base.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("base"))}
	_, r1, e1 := ws.Commit(revision.CommitParams{TreeID: ws.WriteTree(tree), Description: "base", Author: store.Author{Name: "t", Email: "t@x"}})
	if e1 != nil {
		t.Fatal(e1)
	}
	if _, err := ws.SetRef("main", store.RefBranch, r1.ID); err != nil {
		t.Fatal(err)
	}
	srv := stubRemoteServer2(t, remoteRepo)
	defer srv.Close()

	setHome(t)
	localRepo := createRepoIn(t, "demo", "source")
	if err := localRepo.PutRemote("origin", srv.URL, ""); err != nil {
		t.Fatal(err)
	}
	// Set the remote default branch to "main".
	if err := localRepo.SetRemoteDefaultBranch("origin", "main"); err != nil {
		t.Fatal(err)
	}

	if err := doPull(localRepo, []string{"origin"}); err != nil {
		t.Fatalf("doPull: %v", err)
	}

	ref, err := localRepo.GetRef("main")
	if err != nil {
		t.Fatalf("main branch should exist after pull: %v", err)
	}
	if ref.Target == "" {
		t.Fatal("main should point at a revision after pull")
	}
	if tip, _ := localRepo.GetLastSyncTip("origin"); tip == "" {
		t.Fatalf("last_sync_tip should be recorded after pull")
	}
}

func stubRemoteServer2(t *testing.T, remoteRepo *store.Repo) *httptest.Server {
	t.Helper()
	b, err := transfer.CollectAll(remoteRepo)
	if err != nil {
		t.Fatal(err)
	}
	refs, _ := remoteRepo.ListRefs()
	revs, _ := remoteRepo.ListRevisions()
	var ids []string
	for _, r := range revs {
		ids = append(ids, r.ID)
	}
	return newRemoteMux(t, refs, ids, b, "demo/source")
}

// newRemoteMux builds an httptest server that answers advertise+fetch using a
// pre-built bundle. It mirrors the easylab smart-protocol path
// /repo/{ns}/{name}/…
func newRemoteMux(t *testing.T, refs []*store.Ref, ids []string, b *transfer.Bundle, repo string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repo/"+repo+"/advertise", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(advertiseResp{Repo: repo, Changes: ids, Refs: refs})
	})
	mux.HandleFunc("POST /repo/"+repo+"/fetch", func(w http.ResponseWriter, r *http.Request) {
		enc, err := transfer.CompressBundle(b)
		if err != nil {
			t.Fatalf("compress: %v", err)
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(enc)
	})
	return httptest.NewServer(mux)
}
