package revision

import (
	"os"
	"path/filepath"
	"testing"

	"easyvcs/internal/ignore"
	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

func TestBuildTreeFromFSWithMatcherAndIsIgnored(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "keep.txt", "k\n")
		buildFS(t, dir, "tmp.tmp", "t\n")
		m := ignore.NewWithLines(".", "*.tmp")
		treeID, err := ws.BuildTreeFromFSWithMatcher(dir, m)
		if err != nil {
			t.Fatal(err)
		}
		tree := ws.mustTree(treeID)
		if _, ok := tree.Entries["keep.txt"]; !ok {
			t.Fatal("keep should exist")
		}
		if _, ok := tree.Entries["tmp.tmp"]; ok {
			t.Fatal("tmp should be ignored")
		}
		if !IsIgnored(m, "x.tmp") || IsIgnored(m, "x.txt") {
			t.Fatal("IsIgnored mismatch")
		}
	})
}

func TestParentsAndSnapshotID(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "f.txt", "a\n")
		snap1, ch1 := commitTree(t, ws, dir, "c1", nil)
		buildFS(t, dir, "f.txt", "a\nb\n")
		_, ch2 := commitTree(t, ws, dir, "c2", []object.ID{snap1.RevisionHash})

		parents, err := ws.ParentsOfRevision(ch2.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(parents) != 1 || parents[0] != snap1.RevisionHash {
			t.Fatalf("parents: %v", parents)
		}
		p2, _ := ws.ParentsOfChange(ch1.ID)
		if len(p2) != 0 {
			t.Fatalf("root should have no parents, got %v", p2)
		}
		if SnapshotID(snap1) != snap1.RevisionHash {
			t.Fatal("SnapshotID mismatch")
		}
	})
}

func TestAllSnapshotsAndResolveRevisions(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "f.txt", "a\n")
		snap1, ch1 := commitTree(t, ws, dir, "c1", nil)
		buildFS(t, dir, "f.txt", "a\nb\n")
		snap2, ch2 := commitTree(t, ws, dir, "c2", []object.ID{snap1.RevisionHash})

		byRev, byID, err := ws.AllSnapshots()
		if err != nil {
			t.Fatal(err)
		}
		if len(byRev) != 2 || len(byID) != 2 {
			t.Fatalf("snapshots: %d/%d", len(byRev), len(byID))
		}
		if byID[snap2.RevisionHash] != ch2.ID {
			t.Fatal("byID mapping wrong")
		}

		// ResolveRevisions: "@" -> newest, id prefix, "@-"
		ids, _, err := ws.ResolveRevisions([]string{"@"})
		if err != nil || len(ids) != 1 {
			t.Fatalf("resolve @: %v %d", err, len(ids))
		}
		if ids[0] != snap2.RevisionHash {
			t.Fatal("@ should resolve to newest snapshot")
		}
		// by revision id prefix
		ids2, _, _ := ws.ResolveRevisions([]string{ch1.ID[:6]})
		if len(ids2) != 1 || ids2[0] != snap1.RevisionHash {
			t.Fatalf("resolve prefix: %v", ids2)
		}
		// "@-"
		ids3, _, _ := ws.ResolveRevisions([]string{"@-"})
		if len(ids3) != 1 || ids3[0] != snap1.RevisionHash {
			t.Fatalf("resolve @-: %v", ids3)
		}
		// GetRevision + GetChange alias
		if _, err := ws.GetRevision(ch1.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := ws.GetChange(ch1.ID); err != nil {
			t.Fatal(err)
		}
		// Missing revision
		if _, err := ws.GetRevision("nope"); err == nil {
			t.Fatal("expected ErrRevisionNotFound")
		}
	})
}

func TestIgnoredHelpers(t *testing.T) {
	if HasIgnoresCompiled(nil) {
		t.Fatal("nil matcher should not count as has ignores")
	}
	_ = pathKey("x")
}

func TestIgnoreHashAndRebuildTreeFiltered(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "keep.txt", "k\n")
		buildFS(t, dir, "junk.log", "j\n")
		buildFS(t, dir, ".gitignore", "*.log\n")
		treeID, err := ws.BuildTreeFromFS(dir)
		if err != nil {
			t.Fatal(err)
		}
		h, err := IgnoreHash(dir)
		if err != nil {
			t.Fatal(err)
		}
		if h == "" {
			t.Fatal("ignore hash empty")
		}
		// Rebuild after reading the tree with matcher applied
		m, _ := ignore.New(dir)
		newID, err := ws.RebuildTreeFiltered(dir, m, treeID)
		if err != nil {
			t.Fatal(err)
		}
		tree := ws.mustTree(newID)
		if _, ok := tree.Entries["junk.log"]; ok {
			t.Fatal("junk.log should be gone")
		}
		if _, ok := tree.Entries["keep.txt"]; !ok {
			t.Fatal("keep.txt should remain")
		}
	})
}

func TestFlattenAndCount(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "sub/a.txt", "a\n")
		buildFS(t, dir, "sub/b.txt", "b\n")
		buildFS(t, dir, "root.txt", "r\n")
		treeID, err := ws.BuildTreeFromFS(dir)
		if err != nil {
			t.Fatal(err)
		}
		// Diff against empty tree -> all added, flattenTree covers nested.
		added, err := ws.Diff(treeID, ws.writeTree(object.NewTree()))
		if err != nil {
			t.Fatal(err)
		}
		if len(added) != 3 {
			t.Fatalf("flatten added: %d", len(added))
		}
		removed, _ := ws.Diff(ws.writeTree(object.NewTree()), treeID)
		if len(removed) != 3 {
			t.Fatalf("flatten removed: %d", len(removed))
		}
		// countLines: function = count("\n") + 1.
		if countLines([]byte("a\nb\n")) != 3 {
			t.Fatal("countLines mismatch for a\\nb\\n")
		}
		if countLines([]byte("a\nb")) != 2 {
			t.Fatal("countLines mismatch for a\\nb")
		}
		if countLines([]byte("")) != 0 {
			t.Fatal("countLines empty should be 0")
		}
		if countLines(nil) != 0 {
			t.Fatal("countLines nil should be 0")
		}
		// SortFileDiffs
		ds := []FileDiff{{Path: "z"}, {Path: "a"}}
		SortFileDiffs(ds)
		if ds[0].Path != "a" {
			t.Fatal("SortFileDiffs order")
		}
	})
}

func TestScanMarkers(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// A dir with two files then remove one; ComputeChangesFromDir should
		// surface deletion via markDeleted.
		wd := t.TempDir()
		_ = os.WriteFile(filepath.Join(wd, "a.txt"), []byte("a\n"), 0o644)
		_ = os.WriteFile(filepath.Join(wd, "b.txt"), []byte("b\n"), 0o644)
		changes, err := ws.ComputeChangesFromDir(object.ID{}, wd)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) != 2 {
			t.Fatalf("root scan: %d", len(changes))
		}
		snap0, _, _ := ws.CommitFromChanges(object.ID{}, changes, "root", store.Author{Name: "n"}, "")
		_ = os.Remove(filepath.Join(wd, "a.txt"))
		_ = os.WriteFile(filepath.Join(wd, "c.txt"), []byte("c\n"), 0o644)
		changes2, err := ws.ComputeChangesFromDir(snap0.RevisionHash, wd)
		if err != nil {
			t.Fatal(err)
		}
		// deleted a.txt + added c.txt; b.txt unchanged.
		if len(changes2) != 2 {
			t.Fatalf("deltas: %d %+v", len(changes2), changes2)
		}
		var hasDel, hasAdd bool
		for _, c := range changes2 {
			if c.Path == "a.txt" && c.Delete {
				hasDel = true
			}
			if c.Path == "c.txt" && !c.Delete {
				hasAdd = true
			}
		}
		if !hasDel || !hasAdd {
			t.Fatalf("expected del a + add c, got %+v", changes2)
		}
	})
}

func TestMaterializeConflictToFile(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		base := object.NewTree()
		base.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("base"))}
		baseID := ws.writeTree(base)
		ours := object.NewTree()
		ours.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("ours"))}
		oursID := ws.writeTree(ours)
		theirs := object.NewTree()
		theirs.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("theirs"))}
		theirsID := ws.writeTree(theirs)
		merged, atoms, err := ws.Merge(baseID, oursID, theirsID)
		if err != nil {
			t.Fatal(err)
		}
		if len(atoms) != 1 {
			t.Fatalf("atoms: %d", len(atoms))
		}
		// Materialize merged tree to disk -> conflict marker file.
		out := t.TempDir()
		if err := ws.Materialize(merged, out); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(out, "f.txt"))
		if err != nil {
			t.Fatal(err)
		}
		if !contains(string(data), "<<<<<<< conflict") {
			t.Fatalf("expected conflict marker, got:\n%s", data)
		}
	})
}

func TestReadBlobError(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		if _, err := ws.ReadBlob(object.BlobID([]byte("missing"))); err == nil {
			t.Fatal("expected error for missing blob")
		}
	})
}
