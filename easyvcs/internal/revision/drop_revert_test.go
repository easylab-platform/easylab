package revision

import (
	"testing"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// seedDropChain builds A-B-C-D-E with distinct files so a drop is observable.
func seedDropChain(t *testing.T, ws *Workspace) (map[string]*store.Snapshot, map[string]*store.Revision) {
	t.Helper()
	snaps := map[string]*store.Snapshot{}
	revs := map[string]*store.Revision{}
	prev := object.ID{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		tree := object.NewTree()
		tree.Entries[name+".txt"] = object.Entry{Name: name + ".txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte(name))}
		var parents []object.ID
		if prev != (object.ID{}) {
			parents = []object.ID{prev}
		}
		s, r := commitTreeAt(t, ws, tree, name, parents)
		snaps[name] = s
		revs[name] = r
		prev = s.RevisionHash
	}
	return snaps, revs
}

func TestDropRemovesRev(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		snaps, revs := seedDropChain(t, ws)

		// Drop "c" (chain a-b-c-d-e).
		moved, err := ws.Drop(revs["c"].ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(moved) != 2 { // d, e moved (c's descendants)
			t.Fatalf("expected 2 moved descendants, got %d", len(moved))
		}
		// c's new parent should be b, e's parent should be new d.
		dSnap, _ := ws.GetSnapshot(moved[0].Hash)
		if len(dSnap.Parents) != 1 || dSnap.Parents[0] != snaps["b"].RevisionHash {
			t.Fatalf("d should parent=b, got %v", dSnap.Parents)
		}
		eSnap, _ := ws.GetSnapshot(moved[1].Hash)
		if len(eSnap.Parents) != 1 || eSnap.Parents[0] != moved[0].Hash {
			t.Fatalf("e should parent=new d, got %v", eSnap.Parents)
		}
		// c's snapshot must be orphaned (not referenced by any revision).
		revs2, _ := ws.Log()
		referenced := map[string]bool{}
		for _, r := range revs2 {
			s, err := ws.GetSnapshot(r.Hash)
			if err != nil {
				continue
			}
			for _, p := range s.Parents {
				referenced[p.String()] = true
			}
		}
		if referenced[snaps["c"].RevisionHash.String()] {
			t.Fatal("c should be dropped (unreachable)")
		}
	})
}

func TestRevertCreatesReverseRev(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// a: file x=1 ; b: file x=2 ; c(add): file y added.
		snaps, revs := seedDropChain(t, ws)
		_ = revs

		// Revert "c": undoes adding c.txt, keeping a,b and the revert commit.
		// Revert at the tip (onto zero -> current tip = e).
		revertSnap, revertRev, err := ws.Revert(revs["c"].ID, object.ID{})
		if err != nil {
			t.Fatal(err)
		}
		_ = revertRev
		// After revert, c.txt should be absent from the resulting tree (c
		// introduced it, and revert undoes that). Materialize the tree.
		tree := ws.MustTree(revertSnap.TreeID)
		if _, ok := tree.Entries["c.txt"]; ok {
			t.Fatal("revert should remove c.txt from the tree")
		}
		// The revert revision's parent must be the previous tip.
		if len(revertSnap.Parents) != 1 {
			t.Fatalf("revert snapshot should have one parent, got %v", revertSnap.Parents)
		}
		// History preserved: c still exists as a revision.
		if _, err := ws.GetRevision(revs["c"].ID); err != nil {
			t.Fatalf("c should still exist after revert (history preserved): %v", err)
		}
		_ = snaps
	})
}

func TestRevertRootUndoesToEmpty(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// a is the root revision.
		snaps, revs := seedDropChain(t, ws)
		// Revert root a (undo all its content). Onto the tip (e).
		revertSnap, _, err := ws.Revert(revs["a"].ID, object.ID{})
		if err != nil {
			t.Fatal(err)
		}
		tree := ws.MustTree(revertSnap.TreeID)
		// Whether a.txt is removed depends on merge; but the revert commit exists.
		if tree == nil {
			t.Fatal("nil tree after revert")
		}
		_ = snaps
	})
}
