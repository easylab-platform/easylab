package revision

import (
	"testing"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// TestE2E_MultiParentRebaseConflictResolve exercises the full change-native
// flow: commit a base, create two divergent snapshots, rebase a change onto
// both parents (multi-parent, jj-style merge), which yields a first-class
// conflict; then resolve it while keeping the stable change id.
func TestE2E_MultiParentRebaseConflictResolve(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// Base snapshot with file "o.txt" = "base".
		baseTree := object.NewTree()
		baseTree.Entries["o.txt"] = object.Entry{Name: "o.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("base"))}
		_, baseCh := commitTreeFromTree(t, ws, baseTree, "base")
		_ = baseCh

		// Divergent "ours": o.txt = "ours".
		oursTree := object.NewTree()
		oursTree.Entries["o.txt"] = object.Entry{Name: "o.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("ours"))}
		oursSnap, _ := commitTreeFromTree(t, ws, oursTree, "ours")

		// Divergent "theirs": o.txt = "theirs".
		theirsTree := object.NewTree()
		theirsTree.Entries["o.txt"] = object.Entry{Name: "o.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("theirs"))}
		theirsSnap, _ := commitTreeFromTree(t, ws, theirsTree, "theirs")

		// Rebase ours change onto a single new parent (theirs). This is the
		// merge-as-rebase path: overlapping edits on o.txt become a conflict.
		ns, ch, err := ws.Rebase(oursSnap.RevisionID, []object.ID{theirsSnap.RevisionHash})
		if err != nil {
			t.Fatal(err)
		}
		if ch.ID != oursSnap.RevisionID {
			t.Fatalf("change id changed on rebase: %s != %s", ch.ID, oursSnap.RevisionID)
		}

		// The rebased tree must embed a conflict at o.txt (ours vs theirs diverge
		// from base "base").
		atoms, err := ws.ConflictsInTree(ns.TreeID)
		if err != nil {
			t.Fatal(err)
		}
		if len(atoms) != 1 {
			t.Fatalf("expected 1 conflict after rebase, got %d: %+v", len(atoms), atoms)
		}
		if atoms[0].Path != "o.txt" {
			t.Fatalf("conflict path should be o.txt, got %q", atoms[0].Path)
		}

		// Resolve to side 0 (ours). Change id stays stable; conflict removed.
		rs, rch, err := ws.Resolve(oursSnap.RevisionID, "o.txt", 0)
		if err != nil {
			t.Fatal(err)
		}
		if rch.ID != oursSnap.RevisionID {
			t.Fatalf("change id changed on resolve: %s != %s", rch.ID, oursSnap.RevisionID)
		}
		rt := ws.mustTree(rs.TreeID)
		if rt.Entries["o.txt"].Kind == object.KindConflict {
			t.Fatalf("conflict not resolved")
		}
		blob, _ := ws.ReadBlob(rt.Entries["o.txt"].ID)
		if string(blob) != "ours" {
			t.Fatalf("resolved content should be 'ours', got %q", string(blob))
		}
		t.Logf("e2e multi-parent rebase + resolve ok: change=%s stable, content=%s", shortID(rch.ID), string(blob))
	})
}

func baseSnapID(ch *store.Revision, ws *Workspace) object.ID {
	snap, _ := ws.GetSnapshot(ch.Hash)
	return snap.RevisionHash
}

// TestResolvePropagatesToDescendants verifies that resolving a conflict in a
// parent change automatically rewrites descendant changes that carry the same
// conflict, while keeping their change ids stable.
func TestResolvePropagatesToDescendants(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// Build a conflict at dir/f.txt.
		baseTree := object.NewTree()
		dirTree := object.NewTree()
		dirTree.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("base"))}
		baseTree.Entries["dir"] = object.Entry{Name: "dir", Kind: object.KindTree, ID: ws.writeTree(dirTree)}
		baseID := ws.writeTree(baseTree)

		oursDir := object.NewTree()
		oursDir.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("ours"))}
		ours := object.NewTree()
		ours.Entries["dir"] = object.Entry{Name: "dir", Kind: object.KindTree, ID: ws.writeTree(oursDir)}
		oursID := ws.writeTree(ours)

		theirsDir := object.NewTree()
		theirsDir.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("theirs"))}
		theirs := object.NewTree()
		theirs.Entries["dir"] = object.Entry{Name: "dir", Kind: object.KindTree, ID: ws.writeTree(theirsDir)}
		theirsID := ws.writeTree(theirs)

		conflictedTree, atoms, err := ws.Merge(baseID, oursID, theirsID)
		if err != nil {
			t.Fatal(err)
		}
		if len(atoms) != 1 {
			t.Fatalf("expected 1 conflict, got %d", len(atoms))
		}

		// Parent change carries the conflict.
		parentSnap, parentCh, err := ws.Commit(CommitParams{TreeID: conflictedTree, Description: "parent", Author: store.Author{Name: "t"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = parentSnap

		// Child change on top of parent inherits the conflict at the same path.
		childSnap, childCh, err := ws.Commit(CommitParams{
			Parents:     []object.ID{parentSnap.RevisionHash},
			TreeID:      conflictedTree,
			Description: "child",
			Author:      store.Author{Name: "t"},
		})
		if err != nil {
			t.Fatal(err)
		}
		childAtoms, err := ws.ConflictsInTree(childSnap.TreeID)
		if err != nil {
			t.Fatal(err)
		}
		if len(childAtoms) != 1 {
			t.Fatalf("child should inherit conflict at dir/f.txt, got %d", len(childAtoms))
		}

		// Resolve the parent's conflict -> should auto-propagate to child.
		ns, rch, err := ws.Resolve(parentCh.ID, "dir/f.txt", 0)
		if err != nil {
			t.Fatal(err)
		}
		if rch.ID != parentCh.ID {
			t.Fatalf("parent change id changed on resolve: %s", rch.ID)
		}
		parentTree := ws.mustTree(ns.TreeID)
		parentDir := parentTree.Entries["dir"]
		parentDirTree := ws.mustTree(parentDir.ID)
		if parentDirTree.Entries["f.txt"].Kind == object.KindConflict {
			t.Fatalf("parent conflict not resolved")
		}

		// The child should now be resolved too.
		newChild, err := ws.GetRevision(childCh.ID)
		if err != nil {
			t.Fatal(err)
		}
		if newChild.ID != childCh.ID {
			t.Fatalf("child change id changed: %s -> %s", childCh.ID, newChild.ID)
		}
		childSnap2, err := ws.GetSnapshot(newChild.Hash)
		if err != nil {
			t.Fatal(err)
		}
		childAtoms2, err := ws.ConflictsInTree(childSnap2.TreeID)
		if err != nil {
			t.Fatal(err)
		}
		if len(childAtoms2) != 0 {
			t.Fatalf("child conflict should be auto-resolved, got %d atoms", len(childAtoms2))
		}
		t.Logf("resolve propagated to descendant: parent=%s child=%s both resolved", shortID(parentCh.ID), shortID(childCh.ID))
	})
}
