package mirror

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"easyvcs/internal/revision"
	"easyvcs/internal/store"
)

// newTestRepo returns a fresh repo + workspace in a temp dir.
func newTestRepo(t *testing.T) (*store.Repo, *store.CentralStore) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cs.Create(store.RepoRef{Namespace: "team", Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	return repo, cs
}

// seed writes a file into the repo and commits it as a branch tip.
func seed(t *testing.T, repo *store.Repo, path, content string) {
	t.Helper()
	ws := revision.NewWorkspace(repo)
	f := filepath.Join(t.TempDir(), "seed")
	if err := os.MkdirAll(f, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f, path), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	treeID, err := ws.BuildTreeFromFS(f)
	if err != nil {
		t.Fatal(err)
	}
	snap, rev, err := ws.Commit(revision.CommitParams{
		TreeID:      treeID,
		Description: "seed " + path,
		Author:      store.Author{Name: "t", Email: "t@x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = snap
	if err := repo.PutRef(&store.Ref{Name: "main", Kind: store.RefBranch, Target: rev.ID}); err != nil {
		t.Fatal(err)
	}
}

// initBareGit creates a bare git repo and returns its path.
func initBareGit(t *testing.T, dir string) string {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	runGit(t, "", "init", "--bare", "--quiet", dir)
	return dir
}

// initGit creates a non-bare git repo with an initial commit and files.
func initGit(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "init", "--quiet")
	for p, c := range files {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "--quiet", "-m", "init")
	runGit(t, dir, "branch", "-m", "main")
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, string(out))
	}
}

func TestPushToBare(t *testing.T) {
	repo, cs := newTestRepo(t)
	defer cs.Close()
	seed(t, repo, "a.txt", "hello")

	dst := initBareGit(t, "")
	// Push the "main" branch tree as branch "main".
	res, err := Push(context.Background(), repo, PushTarget{Name: "d", URL: dst, Branch: "main"})
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.RevID == "" {
		t.Fatal("empty rev")
	}
	// Verify the destination now contains the file.
	work := t.TempDir()
	runGit(t, work, "clone", "--quiet", "-b", "main", dst, "c")
	if b, err := os.ReadFile(filepath.Join(work, "c", "a.txt")); err != nil || string(b) != "hello" {
		t.Fatalf("pushed content mismatch: %v %q", err, string(b))
	}
}

func TestPushForceOverwrites(t *testing.T) {
	repo, cs := newTestRepo(t)
	defer cs.Close()
	seed(t, repo, "a.txt", "v1")
	dst := initBareGit(t, "")

	if _, err := Push(context.Background(), repo, PushTarget{Name: "d", URL: dst, Branch: "main"}); err != nil {
		t.Fatal(err)
	}

	// Change content and re-push; remote must reflect v2 (force).
	seed(t, repo, "a.txt", "v2")
	if _, err := Push(context.Background(), repo, PushTarget{Name: "d", URL: dst, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	runGit(t, work, "clone", "--quiet", "-b", "main", dst, "c")
	b, _ := os.ReadFile(filepath.Join(work, "c", "a.txt"))
	if string(b) != "v2" {
		t.Fatalf("expected v2, got %q", string(b))
	}
}

func TestPullFromGit(t *testing.T) {
	repo, cs := newTestRepo(t)
	defer cs.Close()

	src := t.TempDir()
	initGit(t, src, map[string]string{"hello.txt": "world"})
	// Track the git path (file URL).
	url := "file://" + src

	rev, err := Pull(context.Background(), repo, PullConfig{URL: url, Branch: "main"})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if rev.ID == "" {
		t.Fatal("empty rev")
	}
	// Resolve via branch.
	ref, err := repo.GetRef("main")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Target != rev.ID {
		t.Fatalf("branch target %q != rev %q", ref.Target, rev.ID)
	}
	// Materialize and read the file.
	ws := revision.NewWorkspace(repo)
	tip, err := repo.GetRevision(rev.ID)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := repo.GetSnapshot(tip.Hash)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	if err := ws.Materialize(snap.TreeID, out); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(out, "hello.txt"))
	if string(b) != "world" {
		t.Fatalf("expected world, got %q", string(b))
	}
}

var _ = time.Now
