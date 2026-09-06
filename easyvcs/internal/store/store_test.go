package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"easyvcs/internal/object"
)

func newTestCentral(t *testing.T) *CentralStore {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestHomeDirDefault(t *testing.T) {
	// When EASYVCS_HOME is unset, HomeDir returns a path under the user home.
	t.Setenv("EASYVCS_HOME", "") // note: setting empty still counts as set in our impl
	os.Unsetenv("EASYVCS_HOME")
	if got := HomeDir(); got == "" {
		t.Fatal("HomeDir should not be empty")
	}
}

func TestHomeDirOverride(t *testing.T) {
	t.Setenv("EASYVCS_HOME", "/override/path")
	if got := HomeDir(); got != "/override/path" {
		t.Fatalf("got %q", got)
	}
}

func TestCreateOpenDeleteList(t *testing.T) {
	cs := newTestCentral(t)
	ref := RepoRef{Namespace: "team", Name: "app"}
	repo, err := cs.Create(ref)
	if err != nil {
		t.Fatal(err)
	}
	if repo.String() != "team/app" {
		t.Fatalf("repo string: %s", repo.String())
	}
	if repo.RepoID() <= 0 {
		t.Fatal("repo id should be positive")
	}
	// Duplicate create fails.
	if _, err := cs.Create(ref); err == nil {
		t.Fatal("expected ErrRepoExists")
	}
	// Open existing.
	got, err := cs.OpenRepo(ref)
	if err != nil {
		t.Fatal(err)
	}
	if got.RepoRef() != ref {
		t.Fatal("repo ref mismatch")
	}
	// Exists.
	if ok, err := cs.RepoExists(ref); err != nil || !ok {
		t.Fatalf("exists: %v %v", ok, err)
	}
	// Open missing.
	if _, err := cs.OpenRepo(RepoRef{Namespace: "x", Name: "y"}); err == nil {
		t.Fatal("expected ErrRepoNotFound")
	}
	// List.
	repos, err := cs.List()
	if err != nil || len(repos) != 1 {
		t.Fatalf("list: %v %d", err, len(repos))
	}
	// Delete.
	if err := cs.Delete(ref); err != nil {
		t.Fatal(err)
	}
	if ok, _ := cs.RepoExists(ref); ok {
		t.Fatal("should be deleted")
	}
}

func TestObjectRoundTripDedup(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	blob := &object.Object{Kind: object.KindBlob, Blob: []byte("hello")}
	if err := repo.WriteObject(blob); err != nil {
		t.Fatal(err)
	}
	// Second write of same content is idempotent.
	if err := repo.WriteObject(blob); err != nil {
		t.Fatal(err)
	}
	got, err := repo.ReadObject(blob.ID())
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Blob) != "hello" {
		t.Fatal("content mismatch")
	}
	if ok, _ := repo.ObjectExists(blob.ID()); !ok {
		t.Fatal("exists should be true")
	}
	if _, err := repo.ReadObject(object.BlobID([]byte("missing"))); err == nil {
		t.Fatal("expected ErrNotFound")
	}
	var count int
	if err := cs.QueryCount(&count); err != nil || count != 1 {
		t.Fatalf("dedup count: %d %v", count, err)
	}
}

func TestWriteObjectsBatch(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	objs := []*object.Object{
		{Kind: object.KindBlob, Blob: []byte("a")},
		{Kind: object.KindBlob, Blob: []byte("b")},
	}
	if err := repo.WriteObjectsBatch(objs); err != nil {
		t.Fatal(err)
	}
	if ok, _ := repo.ObjectExists(objs[0].ID()); !ok {
		t.Fatal("batch a not stored")
	}
	// empty is a no-op
	if err := repo.WriteObjectsBatch(nil); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRevisionCRUD(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	snap := &Snapshot{
		RevisionHash: object.BlobID([]byte("h")),
		RevisionID:   "rev1",
		TreeID:       object.BlobID([]byte("t")),
		Description:  "msg",
		Author:       Author{Name: "n", Email: "e"},
		CommitTime:   time.Now().UTC(),
	}
	if err := repo.PutSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetSnapshot(snap.RevisionHash)
	if err != nil {
		t.Fatal(err)
	}
	if got.RevisionID != "rev1" || got.Description != "msg" || got.Author.Name != "n" {
		t.Fatalf("snapshot mismatch: %+v", got)
	}

	rev := &Revision{ID: "rev1", Hash: object.BlobID([]byte("h")), Created: time.Now().UTC(), ChangedPaths: []string{"a", "b"}}
	if err := repo.PutRevision(rev); err != nil {
		t.Fatal(err)
	}
	gotRev, err := repo.GetRevision("rev1")
	if err != nil {
		t.Fatal(err)
	}
	if gotRev.Hash != rev.Hash || len(gotRev.ChangedPaths) != 2 {
		t.Fatalf("rev mismatch: %+v", gotRev)
	}
	// UpdateRevisionHash preserves ChangedPaths.
	if err := repo.UpdateRevisionHash("rev1", object.BlobID([]byte("h2"))); err != nil {
		t.Fatal(err)
	}
	gotRev, _ = repo.GetRevision("rev1")
	if gotRev.Hash.String() != object.BlobID([]byte("h2")).String() {
		t.Fatal("hash not updated")
	}
	if len(gotRev.ChangedPaths) != 2 {
		t.Fatal("changed paths lost on update")
	}
	// Missing.
	if _, err := repo.GetRevision("nope"); err == nil {
		t.Fatal("expected ErrNotFound")
	}
	// List.
	revs, err := repo.ListRevisions()
	if err != nil || len(revs) != 1 {
		t.Fatalf("list revs: %v %d", err, len(revs))
	}
	// Missing snapshot.
	if _, err := repo.GetSnapshot(object.BlobID([]byte("nope"))); err == nil {
		t.Fatal("expected ErrNotFound for snapshot")
	}
}

func TestRefsCRUD(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	if err := repo.PutRef(&Ref{Name: "main", Kind: RefBranch, Target: "r1"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.PutRef(&Ref{Name: "v1", Kind: RefTag, Target: "r1"}); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetRef("main")
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != RefBranch || got.Target != "r1" {
		t.Fatalf("ref mismatch: %+v", got)
	}
	refs, _ := repo.ListRefs()
	if len(refs) != 2 {
		t.Fatalf("elems: %d", len(refs))
	}
	if err := repo.DeleteRef("main"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetRef("main"); err == nil {
		t.Fatal("deleted ref still present")
	}
	if err := repo.DeleteRef("nonexistent"); err == nil {
		t.Fatal("expected ErrNotFound on delete missing")
	}
}

func TestRemotesCRUD(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	if err := repo.PutRemote("origin", "http://x", "tok123"); err != nil {
		t.Fatal(err)
	}
	rem, err := repo.GetRemote("origin")
	if err != nil {
		t.Fatal(err)
	}
	if rem.URL != "http://x" || rem.Token != "tok123" {
		t.Fatalf("remote mismatch: %+v", rem)
	}
	// update
	if err := repo.PutRemote("origin", "http://y", ""); err != nil {
		t.Fatal(err)
	}
	rem, _ = repo.GetRemote("origin")
	if rem.URL != "http://y" || rem.Token != "" {
		t.Fatalf("updated remote mismatch: %+v", rem)
	}
	list, _ := repo.ListRemotes()
	if len(list) != 1 {
		t.Fatalf("list: %d", len(list))
	}
	if err := repo.DeleteRemote("origin"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetRemote("origin"); err == nil {
		t.Fatal("deleted remote still present")
	}
}

func TestWorkspacesCRUD(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	if err := cs.PutWorkspace(&WorkspaceRow{Path: "/home/x", Repo: repo.RepoRef(), CurrentRevision: "r1", Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	row, err := cs.GetWorkspaceByPath("/home/x")
	if err != nil {
		t.Fatal(err)
	}
	if row.CurrentRevision != "r1" || row.Branch != "main" || row.Repo != repo.RepoRef() {
		t.Fatalf("workspace mismatch: %+v", row)
	}
	ws, _ := cs.ListWorkspaces()
	if len(ws) != 1 {
		t.Fatalf("ws list: %d", len(ws))
	}
	if _, err := cs.GetWorkspaceByPath("/nope"); err == nil {
		t.Fatal("expected ErrNotFound")
	}
}

func TestTxWrites(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	tx, err := repo.BeginTx()
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.WriteObjectsBatchTx(tx, []*object.Object{{Kind: object.KindBlob, Blob: []byte("tx")}}); err != nil {
		t.Fatal(err)
	}
	snap := &Snapshot{
		RevisionHash: object.BlobID([]byte("txh")), RevisionID: "txrev",
		TreeID: object.BlobID([]byte("txt")), Description: "d",
		Author: Author{Name: "n"}, CommitTime: time.Now().UTC(),
	}
	if err := repo.PutSnapshotTx(tx, snap); err != nil {
		t.Fatal(err)
	}
	rev := &Revision{ID: "txrev", Hash: snap.RevisionHash, Created: time.Now().UTC(), ChangedPaths: []string{"a"}}
	if err := repo.PutRevisionTx(tx, rev); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.GetSnapshot(snap.RevisionHash); got == nil {
		t.Fatal("tx snapshot missing after commit")
	}
	if got, _ := repo.GetRevision("txrev"); got == nil {
		t.Fatal("tx revision missing after commit")
	}
}

func TestSetWAL(t *testing.T) {
	cs := newTestCentral(t)
	if err := cs.SetWAL(); err != nil {
		t.Fatal(err)
	}
}

func TestDBPathOverride(t *testing.T) {
	t.Setenv("EASYVCS_HOME", "/tmp/opencode/evhome_test")
	os.MkdirAll("/tmp/opencode/evhome_test", 0o755)
	if got := DBPath(); got != filepath.Join("/tmp/opencode/evhome_test", DefaultDBFile) {
		t.Fatalf("dbpath: %s", got)
	}
}

func TestRepoRefString(t *testing.T) {
	r := RepoRef{Namespace: "a", Name: "b"}
	if r.String() != "a/b" {
		t.Fatal("bad RepoRef.String")
	}
}

func TestAuthorString(t *testing.T) {
	a := Author{Name: "Alice", Email: "a@b"}
	if a.String() != "Alice <a@b>" {
		t.Fatalf("got %q", a.String())
	}
	a2 := Author{Name: "OnlyName"}
	if a2.String() != "OnlyName" {
		t.Fatalf("got %q", a2.String())
	}
}

func TestHelperFuncs(t *testing.T) {
	if got := idsToStrs([]object.ID{object.BlobID([]byte("x"))}); len(got) != 1 || got[0] != object.BlobID([]byte("x")).String() {
		t.Fatalf("idsToStrs: %v", got)
	}
	if _, err := idFromStr(object.BlobID([]byte("x")).String()); err != nil {
		t.Fatal(err)
	}
	if privatePlaceholder() != "" {
		t.Fatalf("privatePlaceholder returned %q", privatePlaceholder())
	}
}
