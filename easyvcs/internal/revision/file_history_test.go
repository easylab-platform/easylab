package revision

import (
	"testing"

	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

func TestFileHistoryLinear(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// Create a linear chain r1 -> r2 -> r3 that modifies f.txt.
		buildFS(t, dir, "f.txt", "v1\n")
		s1, r1 := commitTree(t, ws, dir, "c1", nil)

		buildFS(t, dir, "f.txt", "v2\n")
		s2, r2 := commitTree(t, ws, dir, "c2", []object.ID{s1.RevisionHash})

		buildFS(t, dir, "f.txt", "v3\n")
		s3, r3 := commitTree(t, ws, dir, "c3", []object.ID{s2.RevisionHash})

		// Also add an unrelated file in r3 (should not affect f.txt history).
		buildFS(t, dir, "other.txt", "o\n")
		_, _ = commitTree(t, ws, dir, "c4", []object.ID{s3.RevisionHash})

		// f.txt history from the tip should include r1,r2,r3 but NOT r4.
		edits, err := ws.FileHistory("", "f.txt")
		if err != nil {
			t.Fatal(err)
		}
		if len(edits) != 3 {
			t.Fatalf("expected 3 edits for f.txt, got %d: %+v", len(edits), edits)
		}
		// r2's changed_paths should include f.txt.
		if !containsPath(r2.ChangedPaths, "f.txt") {
			t.Fatalf("r2 ChangedPaths should contain f.txt, got %v", r2.ChangedPaths)
		}
		t.Logf("file history ok: %d edits (r1=%s r2=%s r3=%s)", len(edits), shortID(r1.ID), shortID(r2.ID), shortID(r3.ID))
	})
	_ = object.ID{}
	_ = store.Author{}
}

func TestFileHistoryCount(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "a.txt", "x\n")
		s1, _ := commitTree(t, ws, dir, "c1", nil)
		buildFS(t, dir, "a.txt", "y\n")
		s2, _ := commitTree(t, ws, dir, "c2", []object.ID{s1.RevisionHash})
		buildFS(t, dir, "a.txt", "y\n") // same content on amend-like new commit
		s3, _ := commitTree(t, ws, dir, "c3", []object.ID{s2.RevisionHash})
		_ = s3
		edits, err := ws.FileHistory("", "a.txt")
		if err != nil {
			t.Fatal(err)
		}
		// r2 changed content, r3 did NOT change content -> count only content
		// changes.
		if len(edits) != 2 {
			t.Fatalf("expected 2 content-changing edits, got %d: %+v", len(edits), edits)
		}
		t.Logf("count ok: %d content changes", len(edits))
	})
}

func containsPath(paths []string, p string) bool {
	for _, x := range paths {
		if x == p {
			return true
		}
	}
	return false
}
