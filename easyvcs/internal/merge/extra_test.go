package merge

import (
	"testing"

	"easyvcs/internal/object"
)

func TestIsTreeHelper(t *testing.T) {
	if isTree(blobEntry("x", []byte("a"))) {
		t.Fatal("blob must not be tree")
	}
	if !isTree(treeEntry("d", object.NewTree())) {
		t.Fatal("tree must be tree")
	}
}

func TestJoinPathBoth(t *testing.T) {
	if joinPath("", "x") != "x" {
		t.Fatal("empty parent")
	}
	if joinPath("a", "b") != "a/b" {
		t.Fatal("nested")
	}
}

func TestMergeEntryReadTreeBroken(t *testing.T) {
	ops := &fakeTreeOps{trees: map[string]*object.Tree{}}
	base := object.NewTree()
	base.Entries["d"] = object.Entry{Name: "d", Kind: object.KindTree, ID: object.BlobID([]byte("missing"))}
	ours := object.NewTree()
	theirs := object.NewTree()
	_, _ = Trees(base, ours, theirs, ops)
}

func TestMergeConflictDeleteVsModifyTheirsModifies(t *testing.T) {
	ops := &fakeTreeOps{}
	base := object.NewTree()
	base.Entries["f"] = blobEntry("f", []byte("base"))
	ours := object.NewTree() // deletes f
	theirs := object.NewTree()
	theirs.Entries["f"] = blobEntry("f", []byte("theirs")) // modifies f
	_, atoms := Trees(base, ours, theirs, ops)
	if len(atoms) != 1 {
		t.Fatalf("expected 1 conflict, got %d", len(atoms))
	}
}
