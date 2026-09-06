package revision

import (
	"testing"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
	"easyvcs/internal/transfer"
)

// newTestRepo returns a repo, its central store, and a workspace.
// It is shared by git/smart-protocol tests in this package.
func newTestRepo2(t *testing.T) (*store.Repo, *store.CentralStore, *Workspace) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cs.Create(store.RepoRef{Namespace: "n", Name: "r"})
	if err != nil {
		t.Fatal(err)
	}
	return repo, cs, NewWorkspace(repo)
}

// TestTransferCollectApplyRoundTrip verifies that Collect + Apply reproduces a
// repository's changes and refs on another repo.
func TestTransferCollectApplyRoundTrip(t *testing.T) {
	repoA, csA, wsA := newTestRepo2(t)
	defer csA.Close()
	// Seed repoA with two changes and a ref.
	baseTree := object.NewTree()
	baseTree.Entries["a.txt"] = object.Entry{Name: "a.txt", Kind: object.KindBlob, ID: wsA.WriteBlob([]byte("one"))}
	_, baseCh := commitTreeFromTree(t, wsA, baseTree, "first")
	tree2 := object.NewTree()
	tree2.Entries["a.txt"] = object.Entry{Name: "a.txt", Kind: object.KindBlob, ID: wsA.WriteBlob([]byte("two"))}
	tree2.Entries["b.txt"] = object.Entry{Name: "b.txt", Kind: object.KindBlob, ID: wsA.WriteBlob([]byte("two"))}
	_, err := wsA.SetRef("main", store.RefBranch, baseCh.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = commitTreeFromTree(t, wsA, tree2, "second")

	// Collect full bundle from repoA.
	b, err := transfer.CollectAll(repoA)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Changes) != 2 {
		t.Fatalf("expected 2 changes, got %d", len(b.Changes))
	}
	if len(b.Refs) != 1 {
		t.Fatalf("expected 1 ref, got %d", len(b.Refs))
	}

	// Apply into a fresh repoB.
	repoB, csB, _ := newTestRepo2(t)
	defer csB.Close()
	// repoB is a second repo in the same central store; use a separate one.
	repoB2, err := csA.Create(store.RepoRef{Namespace: "n", Name: "rB"})
	if err != nil {
		t.Fatal(err)
	}
	n, err := transfer.Apply(repoB2, b)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 applied, got %d", n)
	}
	changes, err := repoB2.ListRevisions()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("repoB2 should have 2 changes, got %d", len(changes))
	}
	refs, _ := repoB2.ListRefs()
	if len(refs) != 1 {
		t.Fatalf("repoB2 should have 1 ref, got %d", len(refs))
	}
	_ = repoB
	t.Logf("transfer round-trip ok: %d changes, %d refs", len(changes), len(refs))
}
