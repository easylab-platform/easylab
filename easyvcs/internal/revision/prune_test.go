package revision

import (
	"os"
	"path/filepath"
	"testing"

	"easyvcs/internal/ignore"
)

// TestPruneUnrelatedSubtrees verifies the dropIgnored fast-path: a subtree whose
// paths are not reachable by any ignore pattern is kept verbatim, and a
// directory that becomes empty after filtering is pruned.
func TestPruneUnrelatedSubtrees(t *testing.T) {
	dir := t.TempDir()
	// A matcher that only excludes "logs/*.log". The "logs" dir is under the
	// pattern's prefix, but src/docs are entirely unaffected.
	buildFS(t, dir, "src/main.go", "package main\n")
	buildFS(t, dir, "logs/app.log", "log\n")
	buildFS(t, dir, "logs/x.txt", "x\n")
	buildFS(t, dir, ".gitignore", "logs/*.log\n")
	buildFS(t, dir, "docs/readme.md", "readme\n")

	m, err := ignore.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ws := wsFromDir(t, dir)
	treeID, err := ws.BuildTreeFromFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Filter; fast-path keeps src/docs verbatim, drops app.log, prunes nothing
	// (x.txt remains) — so the result keeps src, docs, logs.
	newID, err := ws.DropIgnoredPathsFromTree(dir, m, treeID)
	if err != nil {
		t.Fatal(err)
	}
	tr := ws.MustTree(newID)
	names := namesOfTree(tr)
	if !names["src"] || !names["docs"] {
		t.Fatalf("unrelated subtrees src/docs should be kept: %v", names)
	}
	if !names["logs"] {
		t.Fatalf("logs dir should be kept (x.txt not a .log): %v", names)
	}
	logs, err := ws.ReadTree(tr.Entries["logs"].ID)
	if err != nil {
		t.Fatal(err)
	}
	logNames := namesOfTree(logs)
	if logNames["app.log"] {
		t.Fatalf("logs/app.log should be dropped: %v", logNames)
	}
	if !logNames["x.txt"] {
		t.Fatalf("logs/x.txt should be kept: %v", logNames)
	}
}

func wsFromDir(t *testing.T, dir string) *Workspace {
	t.Helper()
	repo, cs := newTestRepo(t)
	_ = cs
	return NewWorkspace(repo)
}

var _ = os.Getenv
var _ = filepath.Join
