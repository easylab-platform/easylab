package revision

import (
	"testing"

	"easyvcs/internal/object"
)

// TestAnnotateLineLevel verifies line-level blame across an edit chain: a
// surviving line keeps its original author, a line added by an edit is
// attributed to that edit's revision.
func TestAnnotateLineLevel(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "f.txt", "one\ntwo\nthree\n")
		s1, r1 := commitTree(t, ws, dir, "add f", nil)

		// Change line "two" -> "TWO" and add a new line: attribution should keep
		// "one"/"three" on r1 and give "TWO"/"four" to r2.
		buildFS(t, dir, "f.txt", "one\nTWO\nthree\nfour\n")
		s2, r2 := commitTree(t, ws, dir, "edit f", []object.ID{s1.RevisionHash})
		_ = r2
		_ = s2

		lines, err := ws.Annotate("", "f.txt")
		if err != nil {
			t.Fatal(err)
		}
		if len(lines) != 4 {
			t.Fatalf("annotate lines: %d", len(lines))
		}
		// Line 1 "one" unchanged -> r1; line 2 "TWO" new -> r2; line 3 "three"
		// unchanged -> r1; line 4 "four" new -> r2.
		if lines[0].RevisionID != r1.ID || lines[0].Content != "one" {
			t.Fatalf("line1: %+v", lines[0])
		}
		if lines[1].RevisionID != r2.ID || lines[1].Content != "TWO" {
			t.Fatalf("line2: %+v", lines[1])
		}
		if lines[2].RevisionID != r1.ID || lines[2].Content != "three" {
			t.Fatalf("line3: %+v", lines[2])
		}
		if lines[3].RevisionID != r2.ID || lines[3].Content != "four" {
			t.Fatalf("line4: %+v", lines[3])
		}
	})
}
