package revision

import (
	"os"
	"path/filepath"
	"testing"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

func TestCommitFromChangesRootAndChild(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// Root commit via changes.
		snap0, rev0, err := ws.CommitFromChanges(object.ID{}, []FileChangeSpec{
			{Path: "a.txt", Content: []byte("hello\n")},
			{Path: "b.txt", Content: []byte("world\n")},
		}, "root", store.Author{Name: "t"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(rev0.ChangedPaths) != 2 {
			t.Fatalf("root changed paths should be 2, got %v", rev0.ChangedPaths)
		}

		// Child commit: modify a.txt, delete b.txt, add c.txt.
		snap1, rev1, err := ws.CommitFromChanges(snap0.RevisionHash, []FileChangeSpec{
			{Path: "a.txt", Content: []byte("hello2\n")},
			{Path: "b.txt", Delete: true},
			{Path: "sub/c.txt", Content: []byte("nested\n")},
		}, "child", store.Author{Name: "t"}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(rev1.ChangedPaths) != 3 {
			t.Fatalf("child changed paths should be 3, got %v", rev1.ChangedPaths)
		}
		// parent linkage
		if len(snap1.Parents) != 1 || snap1.Parents[0] != snap0.RevisionHash {
			t.Fatalf("child parent should be root snapshot")
		}
		// Verify content of a.txt in the child tree.
		blob, present, err := ws.pathBlobHash(snap1.TreeID, "sub/c.txt")
		if err != nil || !present {
			t.Fatalf("sub/c.txt missing: %v %v", blob, err)
		}
		data, _ := ws.ReadBlob(blob)
		if string(data) != "nested\n" {
			t.Fatalf("nested content mismatch: %q", data)
		}
		t.Logf("commit-from-changes ok: root=%s child=%s", shortID(rev0.ID), shortID(rev1.ID))
	})
}

func TestComputeChangesFromDir(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// Create a working dir with files.
		wd := t.TempDir()
		_ = os.WriteFile(filepath.Join(wd, "keep.txt"), []byte("keep\n"), 0o644)
		_ = os.WriteFile(filepath.Join(wd, "old.txt"), []byte("old\n"), 0o644)

		// Root: no parent -> all added.
		changes, err := ws.ComputeChangesFromDir(object.ID{}, wd)
		if err != nil {
			t.Fatal(err)
		}
		if len(changes) != 2 {
			t.Fatalf("root changes should be 2, got %d", len(changes))
		}

		// Commit those changes to have a parent.
		snap0, _, err := ws.CommitFromChanges(object.ID{}, changes, "root", store.Author{Name: "t"}, "")
		if err != nil {
			t.Fatal(err)
		}
		// Now modify one, delete another, add a new one.
		_ = os.WriteFile(filepath.Join(wd, "new.txt"), []byte("new\n"), 0o644)
		_ = os.Remove(filepath.Join(wd, "old.txt"))
		changes2, err := ws.ComputeChangesFromDir(snap0.RevisionHash, wd)
		if err != nil {
			t.Fatal(err)
		}
		// 1 added (new.txt) + 1 deleted (old.txt); keep.txt unchanged -> 2 changes.
		if len(changes2) != 2 {
			t.Fatalf("expected 2 changes (add new, delete old), got %d: %+v", len(changes2), changes2)
		}
		hasNew, hasDel := false, false
		for _, c := range changes2 {
			if c.Path == "new.txt" && !c.Delete {
				hasNew = true
			}
			if c.Path == "old.txt" && c.Delete {
				hasDel = true
			}
		}
		if !hasNew || !hasDel {
			t.Fatalf("expected add new.txt and delete old.txt, got %+v", changes2)
		}
		t.Logf("compute changes ok: %d", len(changes2))
	})
}

func TestCommitFromChangesAtomic(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// A change with an invalid nested path shouldn't break the atomic commit
		// for a valid one when the transaction rolls back. We cannot easily
		// force a partial failure, so we check that a bad parent hash errors and
		// does not create a revision.
		_, _, err := ws.CommitFromChanges(object.BlobID([]byte("nonexistent")), []FileChangeSpec{
			{Path: "a.txt", Content: []byte("x")},
		}, "bad", store.Author{Name: "t"}, "")
		if err == nil {
			t.Fatal("expected error for unknown parent hash")
		}
		// No revision should have been created.
		revs, _ := ws.Log()
		if len(revs) != 0 {
			t.Fatalf("atomic commit should not leave a partial revision, got %d", len(revs))
		}
	})
}
