package merge

import (
	"fmt"
	"testing"

	"easyvcs/internal/object"
)

// fakeTreeOps implements treeOps in memory for unit testing merge logic.
type fakeTreeOps struct {
	blobs map[string]*object.Object
	trees map[string]*object.Tree
}

func (f *fakeTreeOps) ReadBlob(id object.ID) ([]byte, error) {
	o, ok := f.blobs[id.String()]
	if !ok {
		return nil, fmt.Errorf("blob %s not found", id)
	}
	return o.Blob, nil
}
func (f *fakeTreeOps) ReadTree(id object.ID) (*object.Tree, error) {
	t, ok := f.trees[id.String()]
	if !ok {
		return nil, fmt.Errorf("tree %s not found", id)
	}
	return t, nil
}
func (f *fakeTreeOps) WriteBlob(data []byte) object.ID { return object.BlobID(data) }
func (f *fakeTreeOps) WriteTree(t *object.Tree) object.ID {
	// Return the tree's own content id (real), and record it for reads.
	id := t.ID()
	f.trees[id.String()] = t
	return id
}
func (f *fakeTreeOps) WriteConflict(c *object.Conflict) object.ID {
	return object.BlobID([]byte("conflict"))
}

func blobEntry(name string, data []byte) object.Entry {
	return object.Entry{Name: name, Kind: object.KindBlob, ID: object.BlobID(data)}
}
func treeEntry(name string, t *object.Tree) object.Entry {
	return object.Entry{Name: name, Kind: object.KindTree, ID: t.ID()}
}

func TestMergeIdenticalAdds(t *testing.T) {
	ops := &fakeTreeOps{}
	base := object.NewTree()
	ours := object.NewTree()
	theirs := object.NewTree()
	ours.Entries["x"] = blobEntry("x", []byte("same"))
	theirs.Entries["x"] = blobEntry("x", []byte("same"))
	merged, atoms := Trees(base, ours, theirs, ops)
	if len(atoms) != 0 {
		t.Fatalf("expected no conflicts, got %d", len(atoms))
	}
	if _, ok := merged.Entries["x"]; !ok {
		t.Fatal("x should be present")
	}
}

func TestMergeSingleSideAdd(t *testing.T) {
	ops := &fakeTreeOps{}
	base := object.NewTree()
	ours := object.NewTree()
	theirs := object.NewTree()
	ours.Entries["only-ours"] = blobEntry("only-ours", []byte("o"))
	theirs.Entries["only-theirs"] = blobEntry("only-theirs", []byte("t"))
	merged, atoms := Trees(base, ours, theirs, ops)
	if len(atoms) != 0 {
		t.Fatalf("expected no conflicts, got %d", len(atoms))
	}
	if _, ok := merged.Entries["only-ours"]; !ok {
		t.Fatal("only-ours missing")
	}
	if _, ok := merged.Entries["only-theirs"]; !ok {
		t.Fatal("only-theirs missing")
	}
}

func TestMergeOneSideMatchesBase(t *testing.T) {
	ops := &fakeTreeOps{}
	base := object.NewTree()
	base.Entries["x"] = blobEntry("x", []byte("base"))
	ours := object.NewTree()
	ours.Entries["x"] = blobEntry("x", []byte("base")) // ours unchanged
	theirs := object.NewTree()
	theirs.Entries["x"] = blobEntry("x", []byte("theirs"))
	merged, atoms := Trees(base, ours, theirs, ops)
	if len(atoms) != 0 {
		t.Fatalf("expected no conflict, got %d", len(atoms))
	}
	if merged.Entries["x"].ID != theirs.Entries["x"].ID {
		t.Fatal("should pick theirs when ours == base")
	}
}

func TestMergeConflictBothModified(t *testing.T) {
	ops := &fakeTreeOps{}
	base := object.NewTree()
	base.Entries["f"] = blobEntry("f", []byte("base"))
	ours := object.NewTree()
	ours.Entries["f"] = blobEntry("f", []byte("ours"))
	theirs := object.NewTree()
	theirs.Entries["f"] = blobEntry("f", []byte("theirs"))
	merged, atoms := Trees(base, ours, theirs, ops)
	if len(atoms) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(atoms))
	}
	if atoms[0].Path != "f" {
		t.Fatalf("conflict path should be f, got %q", atoms[0].Path)
	}
	if merged.Entries["f"].Kind != object.KindConflict {
		t.Fatalf("merged entry should be conflict, got %v", merged.Entries["f"].Kind)
	}
}

func TestMergeAddedVsDeleted(t *testing.T) {
	ops := &fakeTreeOps{}
	base := object.NewTree()
	base.Entries["f"] = blobEntry("f", []byte("base"))
	ours := object.NewTree()   // deletes f
	theirs := object.NewTree() // modifies f
	theirs.Entries["f"] = blobEntry("f", []byte("theirs"))
	merged, atoms := Trees(base, ours, theirs, ops)
	if len(atoms) != 1 {
		t.Fatalf("expected conflict (delete vs modify), got %d", len(atoms))
	}
	_ = merged
}

func TestMergeDeleteBothSides(t *testing.T) {
	ops := &fakeTreeOps{}
	base := object.NewTree()
	base.Entries["f"] = blobEntry("f", []byte("base"))
	// To simulate deletion on both: entries simply absent in ours and theirs.
	ours := object.NewTree()
	theirs := object.NewTree()
	merged, atoms := Trees(base, ours, theirs, ops)
	if len(atoms) != 0 {
		t.Fatalf("expected no conflict when both delete, got %d", len(atoms))
	}
	if _, ok := merged.Entries["f"]; ok {
		t.Fatal("f should be removed")
	}
}

func TestMergeRecursiveTree(t *testing.T) {
	ops := &fakeTreeOps{trees: map[string]*object.Tree{}}
	// base dir/a.txt, ours dir/a.txt=ours, theirs dir/a.txt=theirs -> nested conflict
	baseDir := object.NewTree()
	baseDir.Entries["a"] = blobEntry("a", []byte("base"))
	base := object.NewTree()
	base.Entries["dir"] = treeEntry("dir", baseDir)
	ops.trees[baseDir.ID().String()] = baseDir

	oursDir := object.NewTree()
	oursDir.Entries["a"] = blobEntry("a", []byte("ours"))
	ours := object.NewTree()
	ours.Entries["dir"] = treeEntry("dir", oursDir)
	ops.trees[oursDir.ID().String()] = oursDir

	theirsDir := object.NewTree()
	theirsDir.Entries["a"] = blobEntry("a", []byte("theirs"))
	theirs := object.NewTree()
	theirs.Entries["dir"] = treeEntry("dir", theirsDir)
	ops.trees[theirsDir.ID().String()] = theirsDir

	merged, atoms := Trees(base, ours, theirs, ops)
	if len(atoms) != 1 {
		t.Fatalf("expected 1 nested conflict, got %d", len(atoms))
	}
	if atoms[0].Path != "dir/a" {
		t.Fatalf("expected path dir/a, got %q", atoms[0].Path)
	}
	_ = merged
}

func TestMergeEntryAsTreeNonTree(t *testing.T) {
	ops := &fakeTreeOps{}
	// entryAsTree on a blob should return (nil, false)
	e := blobEntry("f", []byte("x"))
	tr, ok := entryAsTree(e, ops)
	if ok || tr != nil {
		t.Fatal("entryAsTree on blob should be false/nil")
	}
}

func TestMergeConflictNoBase(t *testing.T) {
	ops := &fakeTreeOps{}
	// Both add different content to same path, no base.
	base := object.NewTree()
	ours := object.NewTree()
	ours.Entries["f"] = blobEntry("f", []byte("o"))
	theirs := object.NewTree()
	theirs.Entries["f"] = blobEntry("f", []byte("t"))
	_, atoms := Trees(base, ours, theirs, ops)
	if len(atoms) != 1 {
		t.Fatalf("expected conflict (both add, different), got %d", len(atoms))
	}
}

func TestMergeAbsentSides(p *testing.T) {
	ops := &fakeTreeOps{}
	base := object.NewTree()
	base.Entries["f"] = blobEntry("f", []byte("base"))
	ours := object.NewTree()   // delete
	theirs := object.NewTree() // delete too -> but base exists so both-delete
	// Ensure absent handling: both absent -> removed, no conflict already tested.
	// This test hits the "!oursP && !theirsP && baseP" -> nil.
	m, atoms := Trees(base, ours, theirs, ops)
	if len(atoms) != 0 {
		p.Fatalf("both delete should be no conflict, got %d", len(atoms))
	}
	if _, ok := m.Entries["f"]; ok {
		p.Fatal("f should be gone")
	}
}

func TestMergeEntryAsTreeWithReadTreeFailure(t *testing.T) {
	// entryAsTree returns (nil,false) when ReadTree fails (e.g. kind is tree but
	// id not in map).
	ops := &fakeTreeOps{trees: map[string]*object.Tree{}}
	e := object.Entry{Name: "d", Kind: object.KindTree, ID: object.BlobID([]byte("nope"))}
	tr, ok := entryAsTree(e, ops)
	if ok || tr != nil {
		t.Fatal("entryAsTree should fail when tree cannot be read")
	}
}

func TestMergeConflictBothAddedDifferentAndPathPrefix(t *testing.T) {
	// Exercise the conflict path with a nested prefix via recursive Trees, and
	// the "both added different" resolution in a subdir.
	ops := &fakeTreeOps{trees: map[string]*object.Tree{}}
	base := object.NewTree()
	bd := object.NewTree()
	base.Entries["d"] = treeEntry("d", bd)
	ops.trees[bd.ID().String()] = bd

	od := object.NewTree()
	od.Entries["x"] = blobEntry("x", []byte("ours"))
	ours := object.NewTree()
	ours.Entries["d"] = treeEntry("d", od)
	ops.trees[od.ID().String()] = od

	td := object.NewTree()
	td.Entries["x"] = blobEntry("x", []byte("theirs"))
	theirs := object.NewTree()
	theirs.Entries["d"] = treeEntry("d", td)
	ops.trees[td.ID().String()] = td

	_, atoms := Trees(base, ours, theirs, ops)
	if len(atoms) != 1 {
		t.Fatalf("expected 1 nested conflict, got %d", len(atoms))
	}
	if atoms[0].Path != "d/x" {
		t.Fatalf("expected d/x, got %q", atoms[0].Path)
	}
}
