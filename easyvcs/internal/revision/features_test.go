package revision

import (
	"os"
	"path/filepath"
	"testing"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// newTestRepo creates a repo in the central store (an isolated temp home) and
// returns a built workspace plus the marker.
func newTestRepo(t *testing.T) (*store.Repo, *store.CentralStore) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cs.Create(store.RepoRef{Namespace: "test", Name: "r"})
	if err != nil {
		t.Fatal(err)
	}
	return repo, cs
}

// runBoth builds a test closure against the single-file store, using two
// separate repositories to emulate the previous file/sqlite distinction.
func runBoth(t *testing.T, fn func(t *testing.T, ws *Workspace, dir string)) {
	t.Run("repo1", func(t *testing.T) {
		repo, cs := newTestRepo(t)
		defer cs.Close()
		fn(t, NewWorkspace(repo), t.TempDir())
	})
	t.Run("repo2", func(t *testing.T) {
		repo, cs := newTestRepo(t)
		defer cs.Close()
		fn(t, NewWorkspace(repo), t.TempDir())
	})
}

func commitTree(t *testing.T, ws *Workspace, dir string, desc string, parents []object.ID) (*store.Snapshot, *store.Revision) {
	t.Helper()
	treeID, err := ws.BuildTreeFromFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap, ch, err := ws.Commit(CommitParams{
		Parents:     parents,
		TreeID:      treeID,
		Description: desc,
		Author:      store.Author{Name: "t", Email: "t@x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap, ch
}

// commitTreeFromTree commits a pre-built tree object directly. The tree object
// is written to the store first so downstream reads succeed.
func commitTreeFromTree(t *testing.T, ws *Workspace, tree *object.Tree, desc string) (*store.Snapshot, *store.Revision) {
	t.Helper()
	treeID := ws.writeTree(tree)
	snap, ch, err := ws.Commit(CommitParams{
		TreeID:      treeID,
		Description: desc,
		Author:      store.Author{Name: "t", Email: "t@x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap, ch
}

func buildFS(t *testing.T, root, file, content string) {
	t.Helper()
	if file != "" {
		dir := filepath.Join(root, filepath.Dir(file))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, file), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiff(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "a.txt", "base\n")
		snap1, _ := commitTree(t, ws, dir, "first", nil)

		buildFS(t, dir, "a.txt", "changed\n")
		buildFS(t, dir, "new.txt", "new\n")
		snap2, _ := commitTree(t, ws, dir, "second", []object.ID{snap1.RevisionHash})

		changes, err := ws.Diff(snap1.TreeID, snap2.TreeID)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]Status{}
		for _, c := range changes {
			got[c.Path] = c.Status
		}
		if got["a.txt"] != StatusModified {
			t.Fatalf("a.txt expected modified, got %v", got["a.txt"])
		}
		if got["new.txt"] != StatusAdded {
			t.Fatalf("new.txt expected added, got %v", got["new.txt"])
		}
		t.Logf("diff: %+v", got)
	})
}

func TestSquashKeepsParentChangeID(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "f.txt", "base\n")
		baseSnap, baseCh := commitTree(t, ws, dir, "base", nil)

		buildFS(t, dir, "f.txt", "base\nchild\n")
		_, childCh := commitTree(t, ws, dir, "child", []object.ID{baseSnap.RevisionHash})

		ns, parentCh, err := ws.Squash(childCh.ID)
		if err != nil {
			t.Fatal(err)
		}
		if parentCh.ID != baseCh.ID {
			t.Fatalf("parent change id changed on squash: %s != %s", parentCh.ID, baseCh.ID)
		}
		if ns.RevisionHash == baseSnap.RevisionHash {
			t.Fatalf("squash should produce a new snapshot sha")
		}
		changes, err := ws.Diff(baseSnap.TreeID, ns.TreeID)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) == 0 {
			t.Fatalf("expected content change after squash: %+v", changes)
		}
		t.Logf("squash ok: base=%s child=%s new=%s", shortID(baseCh.ID), shortID(childCh.ID), shortID(ns.RevisionHash.String()))
	})
}

func TestRefs(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "f.txt", "hi\n")
		_, ch := commitTree(t, ws, dir, "x", nil)

		if _, err := ws.SetRef("main", store.RefBranch, ch.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := ws.SetRef("v1", store.RefTag, ch.ID); err != nil {
			t.Fatal(err)
		}
		refs, err := ws.ListRefs()
		if err != nil {
			t.Fatal(err)
		}
		if len(refs) != 2 {
			t.Fatalf("expected 2 refs, got %d", len(refs))
		}
		r, err := ws.GetRef("main")
		if err != nil {
			t.Fatal(err)
		}
		if r.Target != ch.ID || r.Kind != store.RefBranch {
			t.Fatalf("bad ref: %+v", r)
		}
		t.Logf("refs ok: %d", len(refs))
	})
}

func TestMaterialize(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "d/x.txt", "hello\n")
		snap, _ := commitTree(t, ws, dir, "hi", nil)

		out := t.TempDir()
		if err := ws.Materialize(snap.TreeID, out); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(out, "d", "x.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "hello\n" {
			t.Fatalf("materialized content mismatch: %q", string(data))
		}
		t.Logf("materialize ok")
	})
}

func TestRenameRef(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		_, ch := commitTree(t, ws, dir, "x", nil)
		if _, err := ws.SetRef("old", store.RefBranch, ch.ID); err != nil {
			t.Fatal(err)
		}
		if err := ws.DeleteRef("old"); err != nil {
			t.Fatal(err)
		}
		if _, err := ws.SetRef("new", store.RefBranch, ch.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := ws.GetRef("old"); err == nil {
			t.Fatal("old ref should be gone")
		}
		if _, err := ws.GetRef("new"); err != nil {
			t.Fatalf("new ref missing: %v", err)
		}
		t.Logf("rename ref ok")
	})
}

func TestRebaseStableChangeID(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "f.txt", "base\n")
		baseSnap, _ := commitTree(t, ws, dir, "base", nil)

		buildFS(t, dir, "f.txt", "base\nchild\n")
		childSnap, childCh := commitTree(t, ws, dir, "child", []object.ID{baseSnap.RevisionHash})

		reb, ch, err := ws.Rebase(childCh.ID, []object.ID{baseSnap.RevisionHash})
		if err != nil {
			t.Fatal(err)
		}
		if ch.ID != childCh.ID {
			t.Fatalf("rebase changed change id")
		}
		if reb.RevisionHash != childSnap.RevisionHash {
			t.Fatalf("rebase onto immediate parent should be a no-op: %s != %s", shortID(reb.RevisionHash.String()), shortID(childSnap.RevisionHash.String()))
		}
		t.Logf("rebase stable: change=%s", shortID(ch.ID))
	})
}

func TestCentralObjectsDedup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	r1, _ := cs.Create(store.RepoRef{Namespace: "a", Name: "r1"})
	r2, _ := cs.Create(store.RepoRef{Namespace: "a", Name: "r2"})

	blob := &object.Object{Kind: object.KindBlob, Blob: []byte("shared content")}
	if err := r1.WriteObject(blob); err != nil {
		t.Fatal(err)
	}
	// Writing the same content in another repo must deduplicate globally.
	if err := r2.WriteObject(blob); err != nil {
		t.Fatal(err)
	}
	exists, err := r2.ObjectExists(blob.ID())
	if err != nil || !exists {
		t.Fatalf("cross-repo object not found: %v", err)
	}
	// Count objects in the db: should be 1.
	var count int
	_ = cs.QueryCount(&count)
	if count != 1 {
		t.Fatalf("expected dedup to 1 object, got %d", count)
	}
	t.Logf("central dedup ok: 1 object shared across 2 repos")
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}
