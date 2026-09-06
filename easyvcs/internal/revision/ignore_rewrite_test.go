package revision

import (
	"testing"

	"easyvcs/internal/ignore"
	"easyvcs/internal/object"
)

// TestRewriteHistoryShortCircuitsNoMatch verifies optimization A: when no
// ignore pattern could reach the repo root, the rewrite is a no-op (0
// snapshots rewritten) even on a long history, avoiding the O(N*M) walk.
func TestRewriteHistoryShortCircuitsNoMatch(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// Build a long linear chain a-b-c-d-e.
		ordered := []string{"a", "b", "c", "d", "e"}
		trees := map[string]*object.Tree{}
		for _, n := range ordered {
			trees[n] = treeWith(map[string]string{n + ".txt": n}, ws)
		}
		_, revs := commitChainFromTree(t, ws, ordered, trees)

		// A matcher that only excludes "zzz/" — no such path exists: nothing to
		// exclude, so HasAnyMatchUnder("") should be false only if the pattern
		// touches no existing branch. (A root-level matcher returns true, so use
		// a nested rule to exercise the short-circuit path.)
		m := ignore.NewWithLines(".", "vendor/")
		rewrote, err := ws.RewriteHistoryWithIgnores(".", m, revs["d"].ID)
		if err != nil {
			t.Fatal(err)
		}
		if rewrote != 0 {
			t.Fatalf("no matching paths should rewrite 0, got %d", rewrote)
		}
	})
}

// TestRewriteHistoryFiltersMatchingAndPrunesEmpty verifies that a directory
// which becomes empty after its files are ignored is pruned, and that the
// rewrite counts only changed snapshots.
func TestRewriteHistoryFiltersMatchingAndPrunesEmpty(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// a: root with keep.txt + logs/app.log (both tracked).
		ta := object.NewTree()
		ta.Entries["keep.txt"] = object.Entry{Name: "keep.txt", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("keep"))}
		logs := object.NewTree()
		logs.Entries["app.log"] = object.Entry{Name: "app.log", Kind: object.KindBlob, ID: ws.WriteBlob([]byte("log"))}
		ta.Entries["logs"] = object.Entry{Name: "logs", Kind: object.KindTree, ID: ws.writeTree(logs)}
		sa, ra := commitTreeFromTree(t, ws, ta, "a")

		// b: child adds nobody; same tree (link to a's tree for cheap identity).
		tb := ws.MustTree(sa.TreeID)
		sb, rb := commitTreeAt(t, ws, tb, "b", []object.ID{sa.RevisionHash})
		_ = sb

		// Now a rule that ignores "**/*.log" (applies anywhere, reaching root).
		m := ignore.NewWithLines(".", "**/*.log")
		rewrote, err := ws.RewriteHistoryWithIgnores(".", m, rb.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rewrote != 2 {
			t.Fatalf("both a and b should be rewritten, got %d", rewrote)
		}
		// The rewritten tip tree must have keep.txt, and logs must be pruned
		// (empty after app.log removed).
		tip, _ := ws.GetRevision(rb.ID)
		tipSnap, err := ws.GetSnapshot(tip.Hash)
		if err != nil {
			t.Fatal(err)
		}
		tr := ws.MustTree(tipSnap.TreeID)
		names := namesOfTree(tr)
		if !names["keep.txt"] {
			t.Fatalf("keep.txt should remain: %v", names)
		}
		// b's tree equals a's tree; the ignored app.log is filtered and the now-
		// empty logs dir is pruned.
		if names["logs"] {
			t.Fatalf("logs dir should be pruned (empty after filtering): %v", names)
		}
		_ = ra
	})
}
