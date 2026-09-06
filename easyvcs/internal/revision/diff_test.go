package revision

import (
	"testing"

	"easyvcs/internal/object"
)

func TestUnifiedDiffBasic(t *testing.T) {
	old := []byte("a\nb\nc\nd\ne\n")
	new := []byte("a\nb\nX\nc\nd\ne\n")
	hunks := UnifiedDiff(old, new)
	if len(hunks) == 0 {
		t.Fatal("expected hunks for a single insertion")
	}
	rendered := RenderUnified("f", "f", hunks)
	if !contains(rendered, "+X") || !contains(rendered, "@@") {
		t.Fatalf("unexpected diff output:\n%s", rendered)
	}
	t.Logf("diff:\n%s", rendered)
}

func TestUnifiedDiffIdentical(t *testing.T) {
	old := []byte("same\ncontent\n")
	hunks := UnifiedDiff(old, old)
	if huns := hunks; huns != nil {
		t.Fatalf("expected nil hunks for identical content, got %d", len(huns))
	}
}

func TestUnifiedDiffDelete(t *testing.T) {
	old := []byte("a\nb\nc\n")
	new := []byte("a\nc\n")
	hunks := UnifiedDiff(old, new)
	rendered := RenderUnified("f", "f", hunks)
	if !contains(rendered, "-b") {
		t.Fatalf("expected deletion of b, got:\n%s", rendered)
	}
}

func TestDiffContentStatus(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// Tree A: one file "f.txt" = "line1\nline2\n"
		treeA := object.NewTree()
		treeA.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("line1\nline2\n"))}
		aID := ws.writeTree(treeA)

		// Tree B: same file modified + a new file.
		treeB := object.NewTree()
		// Modify the middle line (line2 -> CHANGED), and drop "line2" boundary
		// semantics so we get a clean 1-del + 1-add.
		treeB.Entries["f.txt"] = object.Entry{Name: "f.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("line1\nCHANGED\n"))}
		treeB.Entries["new.txt"] = object.Entry{Name: "new.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("hello\n"))}
		bID := ws.writeTree(treeB)

		diffs, err := ws.DiffContent(aID, bID)
		if err != nil {
			t.Fatal(err)
		}
		if len(diffs) != 2 {
			t.Fatalf("expected 2 file diffs, got %d", len(diffs))
		}
		byPath := map[string]FileDiff{}
		for _, d := range diffs {
			byPath[d.Path] = d
		}
		if byPath["f.txt"].Status != StatusModified {
			t.Fatalf("f.txt should be modified, got %v", byPath["f.txt"].Status)
		}
		if byPath["new.txt"].Status != StatusAdded {
			t.Fatalf("new.txt should be added, got %v", byPath["new.txt"].Status)
		}
		if byPath["f.txt"].AddedLines != 1 || byPath["f.txt"].RemovedLines != 1 {
			t.Fatalf("expected 1 added/1 removed for f.txt, got +%d/-%d", byPath["f.txt"].AddedLines, byPath["f.txt"].RemovedLines)
		}
		t.Logf("f.txt diff content:\n%s", byPath["f.txt"].Content)
		if !contains(byPath["f.txt"].Content, "+CHANGED") || !contains(byPath["f.txt"].Content, "-line2") {
			t.Fatalf("unexpected f.txt diff content")
		}
	})
}
