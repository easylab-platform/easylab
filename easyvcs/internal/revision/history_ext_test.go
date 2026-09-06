package revision

import (
	"testing"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

func TestFileHistoryRefDescLimit(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// r1 -> r2 -> r3 all modify f.txt; make r1 the only one that creates it.
		buildFS(t, dir, "f.txt", "v1\n")
		s1, r1 := commitTree(t, ws, dir, "c1", nil)
		buildFS(t, dir, "f.txt", "v2\n")
		s2, r2 := commitTree(t, ws, dir, "c2", []object.ID{s1.RevisionHash})
		buildFS(t, dir, "f.txt", "v3\n")
		s3, r3 := commitTree(t, ws, dir, "c3", []object.ID{s2.RevisionHash})
		_ = s3
		_ = r2

		// Annotate a branch "main" -> r3 tip.
		if _, err := ws.SetRef("main", store.RefBranch, r3.ID); err != nil {
			t.Fatal(err)
		}
		// A tag at r1.
		if _, err := ws.SetRef("v1", store.RefTag, r1.ID); err != nil {
			t.Fatal(err)
		}

		// Desc (newest-first) by default anchor.
		edits, err := ws.FileHistoryOpt(HistoryOpt{Start: "main", Desc: true}, "f.txt")
		if err != nil {
			t.Fatal(err)
		}
		if len(edits) != 3 {
			t.Fatalf("expected 3 edits, got %d", len(edits))
		}
		if edits[0].RevisionID != r3.ID || edits[2].RevisionID != r1.ID {
			t.Fatalf("desc order wrong: %s %s %s", edits[0].RevisionID, edits[1].RevisionID, edits[2].RevisionID)
		}

		// Anchor via tag (only r1 touched up to v1).
		limited, err := ws.FileHistoryOpt(HistoryOpt{Start: "v1"}, "f.txt")
		if err != nil {
			t.Fatal(err)
		}
		if len(limited) != 1 || limited[0].RevisionID != r1.ID {
			t.Fatalf("tag-anchored history wrong: %+v", limited)
		}

		// Limit.
		two, err := ws.FileHistoryOpt(HistoryOpt{Start: "main", Desc: true, Limit: 2}, "f.txt")
		if err != nil {
			t.Fatal(err)
		}
		if len(two) != 2 {
			t.Fatalf("limit: expected 2, got %d", len(two))
		}
	})
	_ = object.ID{}
}

func TestFilesChangedStatus(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "a.txt", "base\n")
		s1, r1 := commitTree(t, ws, dir, "first", nil)
		buildFS(t, dir, "a.txt", "changed\n")
		buildFS(t, dir, "b.txt", "new\n")
		s2, r2 := commitTree(t, ws, dir, "second", []object.ID{s1.RevisionHash})
		_ = s2
		_ = r2

		changes, err := ws.FilesChanged(r2.ID)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]Status{}
		for _, c := range changes {
			got[c.Path] = c.Status
		}
		if got["a.txt"] != StatusModified {
			t.Fatalf("a.txt expected modified, got %v", got)
		}
		if got["b.txt"] != StatusAdded {
			t.Fatalf("b.txt expected added, got %v", got)
		}
		_ = r1
	})
}

func TestRebaseManyMovesSequence(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// Build explicit trees so we can track exact contents.
		treeM := object.NewTree()
		treeM.Entries["m.txt"] = object.Entry{Name: "m.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("m"))}
		_, ra := commitTreeFromTree(t, ws, treeM, "a")
		_ = ra

		treeB := object.NewTree()
		treeB.Entries["m.txt"] = object.Entry{Name: "m.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("m"))}
		treeB.Entries["m2.txt"] = object.Entry{Name: "m2.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("m2"))}
		sb, rb := commitTreeFromTree(t, ws, treeB, "b")

		// feature: b -> d -> g -> h (g is the one to drop).
		treeD := object.NewTree()
		treeD.Entries["d.txt"] = object.Entry{Name: "d.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("d"))}
		sd, rd := commitTreeAt(t, ws, treeD, "d", []object.ID{sb.RevisionHash})

		treeG := object.NewTree()
		treeG.Entries["g.txt"] = object.Entry{Name: "g.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("g"))}
		sg, _ := commitTreeAt(t, ws, treeG, "g", []object.ID{sd.RevisionHash})

		treeH := object.NewTree()
		treeH.Entries["h.txt"] = object.Entry{Name: "h.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("h"))}
		sh, rh := commitTreeAt(t, ws, treeH, "h", []object.ID{sg.RevisionHash})
		_ = sh

		// RebaseMany([d, h]) onto b -> produces b-d-h, dropping g.
		moved, err := ws.RebaseMany([]string{rd.ID, rh.ID}, sb.RevisionHash)
		if err != nil {
			t.Fatal(err)
		}
		if len(moved) != 2 {
			t.Fatalf("expected 2 moved revisions, got %d", len(moved))
		}
		// d must rebase onto b; h must chain onto the new d.
		dSnap, _ := ws.GetSnapshot(moved[0].Hash)
		if len(dSnap.Parents) != 1 || dSnap.Parents[0] != sb.RevisionHash {
			t.Fatalf("d should rebase onto b, got parents %v", dSnap.Parents)
		}
		hSnap, _ := ws.GetSnapshot(moved[1].Hash)
		if len(hSnap.Parents) != 1 || hSnap.Parents[0] != moved[0].Hash {
			t.Fatalf("h should chain onto d, got parents %v", hSnap.Parents)
		}
		// g is now unreachable: no revision parents reference it (sg is orphaned).
		revs, _ := ws.Log()
		referenced := map[string]bool{}
		for _, r := range revs {
			s, err := ws.GetSnapshot(r.Hash)
			if err != nil {
				continue
			}
			for _, p := range s.Parents {
				referenced[p.String()] = true
			}
		}
		if referenced[sg.RevisionHash.String()] {
			t.Fatal("g should be dropped (unreachable) after RebaseMany")
		}
		_ = rb
	})
}

func TestDiffBaseAlwaysFirstParent(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// Linear baseline.
		buildFS(t, dir, "base.txt", "base\n")
		s0, r0 := commitTree(t, ws, dir, "base", nil)

		// Single child: add x.txt.
		buildFS(t, dir, "x.txt", "x\n")
		_, r1 := commitTree(t, ws, dir, "add x", []object.ID{s0.RevisionHash})

		// Diff base for the child must be the first parent's tree exactly.
		base, err := ws.DiffBaseForRevision(r1.ID)
		if err != nil {
			t.Fatal(err)
		}
		if base != s0.TreeID {
			t.Fatalf("diff base should be the parent tree, got %s != %s", base, s0.TreeID)
		}
		// Root revision diff base is the empty tree.
		rootBase, err := ws.DiffBaseForRevision(r0.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !rootBase.IsZero() {
			t.Fatalf("root diff base should be empty, got %s", rootBase)
		}
	})
}

// commitTreeAt commits a pre-built tree with explicit parents.
func commitTreeAt(t *testing.T, ws *Workspace, tree *object.Tree, desc string, parents []object.ID) (*store.Snapshot, *store.Revision) {
	t.Helper()
	snap, ch, err := ws.Commit(CommitParams{
		Parents:     parents,
		TreeID:      ws.writeTree(tree),
		Description: desc,
		Author:      store.Author{Name: "t", Email: "t@x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap, ch
}
