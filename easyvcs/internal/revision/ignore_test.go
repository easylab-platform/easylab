package revision

import (
	"testing"

	"easyvcs/internal/ignore"
	"easyvcs/internal/object"
	"easyvcs/internal/store"
)

// TestBuildTreeFromFSIgnores verifies buildTreeFromFS excludes .gitignore /
// .vcsignore matched files and tree-building round-trips.
func TestBuildTreeFromFSIgnores(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "src/main.go", "package main\n")
		buildFS(t, dir, "src/secret.key", "secret\n")
		buildFS(t, dir, ".gitignore", "*.key\n")
		treeID, err := ws.BuildTreeFromFS(dir)
		if err != nil {
			t.Fatal(err)
		}
		tree := ws.mustTree(treeID)
		sub := ws.mustTree(tree.Entries["src"].ID)
		if _, ok := sub.Entries["secret.key"]; ok {
			t.Fatal("secret.key should be ignored by .gitignore")
		}
		if _, ok := sub.Entries["main.go"]; !ok {
			t.Fatal("main.go should be tracked")
		}
		t.Logf("build tree ignores .gitignore patterns ok")
	})
}

// TestBuildTreeFromFSVcsIgnore verifies .vcsignore is honored too.
func TestBuildTreeFromFSVcsIgnore(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "data/cache.bin", "abc")
		buildFS(t, dir, ".vcsignore", "cache.bin\n")
		treeID, err := ws.BuildTreeFromFS(dir)
		if err != nil {
			t.Fatal(err)
		}
		tree := ws.mustTree(treeID)
		sub := ws.mustTree(tree.Entries["data"].ID)
		if _, ok := sub.Entries["cache.bin"]; ok {
			t.Fatal("cache.bin should be excluded by .vcsignore")
		}
	})
}

// TestDropIgnoredPaths verifies the history-rewrite helper removes ignored
// paths from a tree and prunes empty dirs.
func TestDropIgnoredPaths(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		buildFS(t, dir, "keep.txt", "keep\n")
		buildFS(t, dir, "junk.tmp", "junk\n")
		m := ignore.NewWithLines(".", "*.tmp")
		treeID, err := ws.BuildTreeFromFS(dir)
		if err != nil {
			t.Fatal(err)
		}
		newID, err := ws.DropIgnoredPathsFromTree(dir, m, treeID)
		if err != nil {
			t.Fatal(err)
		}
		nt := ws.mustTree(newID)
		if _, ok := nt.Entries["junk.tmp"]; ok {
			t.Fatal("junk.tmp should be dropped")
		}
		if _, ok := nt.Entries["keep.txt"]; !ok {
			t.Fatal("keep.txt should remain")
		}
	})
}

// TestRewriteHistoryWithIgnores verifies a commit chain's ancestor snapshots are
// rewritten to drop now-ignored paths, and descendant change ids stay stable.
func TestRewriteHistoryWithIgnores(t *testing.T) {
	runBoth(t, func(t *testing.T, ws *Workspace, dir string) {
		// Commit 1 tracks a secret file (ignored by no rules).
		buildFS(t, dir, "a.txt", "a\n")
		buildFS(t, dir, "secret.key", "ss\n")
		snap1, ch1 := commitTree(t, ws, dir, "c1", nil)

		// Commit 2 on top.
		buildFS(t, dir, "b.txt", "b\n")
		_, ch2 := commitTree(t, ws, dir, "c2", []object.ID{snap1.RevisionHash})

		// Now add .gitignore that ignores *.key and rewrite history from ch2.
		buildFS(t, dir, ".gitignore", "*.key\n")
		m, _ := ignore.New(dir)
		rewrote, err := ws.RewriteHistoryWithIgnores(dir, m, ch2.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rewrote != 2 {
			t.Fatalf("expected 2 snapshots rewritten, got %d", rewrote)
		}
		// Both change ids stay stable.
		ch1b, err := ws.GetRevision(ch1.ID)
		if err != nil {
			t.Fatal(err)
		}
		ch2b, err := ws.GetRevision(ch2.ID)
		if err != nil {
			t.Fatal(err)
		}
		if ch1b.ID != ch1.ID || ch2b.ID != ch2.ID {
			t.Fatal("change ids must stay stable after rewrite")
		}
		// The rewritten tip snapshot must not contain secret.key.
		snap2b, _ := ws.GetSnapshot(ch2b.Hash)
		t2 := ws.mustTree(snap2b.TreeID)
		if _, ok := t2.Entries["secret.key"]; ok {
			t.Fatal("secret.key should be removed from rewritten history")
		}
		if _, ok := t2.Entries["a.txt"]; !ok {
			t.Fatal("a.txt should remain")
		}
		t.Logf("rewrite ok: rewrote=%d tip=%s", rewrote, shortID(ch2b.ID))
	})
}

var _ = store.ErrNotFound
