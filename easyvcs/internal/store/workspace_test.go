package store

import (
	"os"
	"path/filepath"
	"testing"

	"easyvcs/internal/object"
)

func TestMarkers(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := LoadMarker(dir); err == nil {
		t.Fatal("expected ErrNotFound when no marker")
	}
	m := &WorkspaceMarker{Repo: RepoRef{Namespace: "n", Name: "r"}, CurrentRevision: "rev1", Branch: "main"}
	if err := WriteMarker(dir, m); err != nil {
		t.Fatal(err)
	}
	got, err := LookupMarker(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Repo != m.Repo || got.CurrentRevision != "rev1" {
		t.Fatalf("marker mismatch: %+v", got)
	}
	// Nested dir loads parent's marker.
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0o755)
	foundDir, got2, err := LoadMarker(sub)
	if err != nil {
		t.Fatal(err)
	}
	if foundDir != dir || got2.CurrentRevision != "rev1" {
		t.Fatalf("nested marker: %s %+v", foundDir, got2)
	}
}

func TestWorkspaceResolve(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	dir := t.TempDir()
	if err := WriteMarker(dir, &WorkspaceMarker{Repo: repo.RepoRef(), CurrentRevision: "r1"}); err != nil {
		t.Fatal(err)
	}
	marker, gotRepo, err := cs.ResolveWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if gotRepo.RepoRef() != repo.RepoRef() || marker.CurrentRevision != "r1" {
		t.Fatalf("resolve mismatch: %+v %+v", marker, gotRepo)
	}
	// Missing repo
	dir2 := t.TempDir()
	WriteMarker(dir2, &WorkspaceMarker{Repo: RepoRef{Namespace: "no", Name: "repo"}})
	if _, _, err := cs.ResolveWorkspace(dir2); err == nil {
		t.Fatal("expected error resolving missing repo")
	}
}

func TestRepoCloseAndObjectIDs(t *testing.T) {
	cs := newTestCentral(t)
	repo, _ := cs.Create(RepoRef{Namespace: "n", Name: "r"})
	_ = repo.WriteObject(&object.Object{Kind: object.KindBlob, Blob: []byte("a")})
	ids, err := repo.ObjectIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("object ids: %d", len(ids))
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestListEmpty(t *testing.T) {
	cs := newTestCentral(t)
	repos, err := cs.List()
	if err != nil || len(repos) != 0 {
		t.Fatalf("expected empty list: %v %d", err, len(repos))
	}
}
