package revision

import (
	"os"
	"path/filepath"
	"testing"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

func blobIDOf(t *testing.T, ws *Workspace, data string) object.ID {
	t.Helper()
	return ws.WriteBlob([]byte(data))
}

// TestMerge3WayConflict verifies the 3-way merge embeds a first-class conflict
// object at the conflicting path (with directory prefix) and that materializing
// the tree yields conflict-marker text in the file.
func TestMerge3WayConflict(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		baseID := writeIfLeaf(ws, object.BlobID([]byte("base")), []byte("base"))
		_ = baseID
		baseTree := object.NewTree()
		dirTree := object.NewTree()
		dirTree.Entries["a.txt"] = object.Entry{Name: "a.txt", Kind: object.KindBlob, ID: blobIDOf(t, ws, "base")}
		baseTree.Entries["dir"] = object.Entry{Name: "dir", Kind: object.KindTree, ID: ws.writeTree(dirTree)}
		btree := ws.writeTree(baseTree)

		oursDir := object.NewTree()
		oursDir.Entries["a.txt"] = object.Entry{Name: "a.txt", Kind: object.KindBlob, ID: blobIDOf(t, ws, "ours")}
		ours := object.NewTree()
		ours.Entries["dir"] = object.Entry{Name: "dir", Kind: object.KindTree, ID: ws.writeTree(oursDir)}
		oursTree := ws.writeTree(ours)

		theirsDir := object.NewTree()
		theirsDir.Entries["a.txt"] = object.Entry{Name: "a.txt", Kind: object.KindBlob, ID: blobIDOf(t, ws, "theirs")}
		theirs := object.NewTree()
		theirs.Entries["dir"] = object.Entry{Name: "dir", Kind: object.KindTree, ID: ws.writeTree(theirsDir)}
		theirsTree := ws.writeTree(theirs)

		merged, atoms, err := ws.Merge(btree, oursTree, theirsTree)
		if err != nil {
			t.Fatal(err)
		}
		if len(atoms) != 1 {
			t.Fatalf("expected 1 conflict atom, got %d: %+v", len(atoms), atoms)
		}
		if atoms[0].Path != "dir/a.txt" {
			t.Fatalf("conflict path should be 'dir/a.txt', got %q", atoms[0].Path)
		}

		// The merged tree must contain a KindConflict entry at the path.
		mt := ws.mustTree(merged)
		dirEntry := mt.Entries["dir"]
		dirTreeObj := ws.mustTree(dirEntry.ID)
		if dirTreeObj.Entries["a.txt"].Kind != object.KindConflict {
			t.Fatalf("expected KindConflict at dir/a.txt, got kind %v", dirTreeObj.Entries["a.txt"].Kind)
		}

		// Materialize the merged tree into a dir and confirm conflict marker text.
		out := t.TempDir()
		if err := ws.Materialize(merged, out); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(out, "dir", "a.txt"))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		if !contains(text, "<<<<<<< conflict") {
			t.Fatalf("expected conflict marker in file, got:\n%s", text)
		}
		t.Logf("merge conflict ok: path=%s marker present, content:\n%s", atoms[0].Path, text)
	})
}

// TestResolveConflictChangeIDStable verifies resolve picks a side, removes the
// conflict from the tree, and keeps the change id unchanged.
func TestResolveConflictChangeIDStable(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		baseTree := object.NewTree()
		baseTree.Entries["a.txt"] = object.Entry{Name: "a.txt", Kind: object.KindBlob, ID: blobIDOf(t, ws, "base")}
		baseTreeID := ws.writeTree(baseTree)

		ours := object.NewTree()
		ours.Entries["a.txt"] = object.Entry{Name: "a.txt", Kind: object.KindBlob, ID: blobIDOf(t, ws, "ours")}
		oursID := ws.writeTree(ours)

		theirs := object.NewTree()
		theirs.Entries["a.txt"] = object.Entry{Name: "a.txt", Kind: object.KindBlob, ID: blobIDOf(t, ws, "theirs")}
		theirsID := ws.writeTree(theirs)

		merged, atoms, err := ws.Merge(baseTreeID, oursID, theirsID)
		if err != nil {
			t.Fatal(err)
		}
		if len(atoms) != 1 {
			t.Fatalf("expected 1 conflict atom, got %d", len(atoms))
		}
		// Commit the conflicted tree as a change.
		snap, ch, err := ws.Commit(CommitParams{TreeID: merged, Description: "conflicted", Author: store.Author{Name: "t"}})
		if err != nil {
			t.Fatal(err)
		}
		_ = snap

		// Resolve the conflict to side 0 (ours).
		ns, ch2, err := ws.Resolve(ch.ID, "a.txt", 0)
		if err != nil {
			t.Fatal(err)
		}
		if ch2.ID != ch.ID {
			t.Fatalf("change id changed on resolve: %s -> %s", ch.ID, ch2.ID)
		}
		// The new tree must have no conflict and contain ours content.
		nt := ws.mustTree(ns.TreeID)
		if nt.Entries["a.txt"].Kind == object.KindConflict {
			t.Fatalf("conflict not resolved")
		}
		blob, err := ws.ReadBlob(nt.Entries["a.txt"].ID)
		if err != nil {
			t.Fatal(err)
		}
		if string(blob) != "ours" {
			t.Fatalf("resolved content should be 'ours', got %q", string(blob))
		}
		t.Logf("resolve ok: change=%s unchanged; content=%s", shortID(ch.ID), string(blob))
	})
}

func contains(s, sub string) bool {
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// writeIfLeaf writes a blob if not already present; unused keeps signature.
func writeIfLeaf(ws *Workspace, id object.ID, data []byte) object.ID {
	_ = ws.WriteBlob(data)
	return id
}
