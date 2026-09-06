// Package mirror implements push/pull mirroring between an EasyVCS
// repository (revision-native) and an external git server over the local
// git CLI (no inbound git smart protocol). A push mirror exports a single
// branch's tree to an external branch; a pull mirror reads a single
// external branch as a read-only snapshot inside EasyVCS.
package mirror

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"easyvcs/internal/object"
	"easyvcs/internal/revision"
	"easyvcs/internal/store"
)

// PushTarget configures one force-push destination bound to a branch.
type PushTarget struct {
	Name    string
	URL     string
	Branch  string // branch name to export (default "main")
	Token   string
	LastRev string // last pushed revision id (may be empty on first push)
}

// PushResult reports the outcome of a single push.
type PushResult struct {
	Target string
	RevID  string
	TreeID string
}

// Push exports the tree of PushTarget.Branch to the external URL with
// `git push --force`. The target branch keeps exactly that tree; any
// divergence on the remote is overwritten (mirror semantics).
func Push(ctx context.Context, repo *store.Repo, t PushTarget) (*PushResult, error) {
	ws := revision.NewWorkspace(repo)

	ref, err := repo.GetRef(t.Branch)
	if err != nil {
		return nil, fmt.Errorf("push mirror: branch %q: %w", t.Branch, err)
	}
	if ref.Kind != store.RefBranch {
		return nil, fmt.Errorf("push mirror: %q is not a branch", t.Branch)
	}
	rev, err := repo.GetRevision(ref.Target)
	if err != nil {
		return nil, fmt.Errorf("push mirror: revision %q: %w", ref.Target, err)
	}
	snap, err := repo.GetSnapshot(rev.Hash)
	if err != nil {
		return nil, err
	}

	tmp, err := os.MkdirTemp("", "easyvcs-push-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	// Materialize the tree into a fresh worktree that is also the git repo,
	// so the exported files live at the repository root.
	work := filepath.Join(tmp, "work")
	msg := snap.Description
	if msg == "" {
		msg = "easyvcs push " + ref.Target
	}
	if err := gitInit(work, t.Token); err != nil {
		return nil, err
	}
	if err := ws.Materialize(snap.TreeID, work); err != nil {
		return nil, err
	}
	if err := run(ctx, work, t.Token, "add", "-A"); err != nil {
		return nil, err
	}
	if err := run(ctx, work, t.Token, "commit", "--quiet", "-m", msg, "--allow-empty"); err != nil {
		return nil, err
	}
	if err := run(ctx, work, t.Token, "push", "--force", t.URL, "HEAD:"+t.Branch); err != nil {
		return nil, err
	}

	return &PushResult{Target: t.Name, RevID: ref.Target, TreeID: snap.TreeID.String()}, nil
}

// PullConfig configures one read-only mirror import of an external branch.
type PullConfig struct {
	URL      string
	Branch   string // external branch to snapshot (default "main")
	Token    string
	Interval int // seconds between scheduled pulls; <=0 = manual only
}

// Pull clones the external branch and records a single snapshot revision for
// it, repositioning the mirror's branch (default branch) onto that
// revision. History is not accumulated: each pull replaces the tip tree, so a
// mirror never exposes a commit-graph of its own (option B).
func Pull(ctx context.Context, repo *store.Repo, cfg PullConfig) (*store.Revision, error) {
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	ws := revision.NewWorkspace(repo)

	tmp, err := os.MkdirTemp("", "easyvcs-pull-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	src, err := clone(ctx, cfg.URL, cfg.Branch, cfg.Token, tmp)
	if err != nil {
		return nil, err
	}
	treeID, err := ws.BuildTreeFromFS(src)
	if err != nil {
		return nil, err
	}

	// Parent = the mirror's current tip snapshot (if any), else none. This
	// keeps the snapshot lineage detectable without exposing an accumulated
	// external history on the read surface.
	var parents []object.ID
	if ref, err := repo.GetRef(cfg.Branch); err == nil && ref != nil {
		if rev, err := repo.GetRevision(ref.Target); err == nil {
			parents = []object.ID{rev.Hash}
		}
	}

	snap, rev, err := ws.Commit(revision.CommitParams{
		Parents:     parents,
		TreeID:      treeID,
		Description: "mirror pull from " + cfg.URL,
		Author:      store.Author{Name: "easyvcs", Email: "easyvcs@example.com"},
	})
	if err != nil {
		return nil, err
	}
	_ = snap

	// Point the mirror's branch to the new revision.
	if err := repo.PutRef(&store.Ref{Name: cfg.Branch, Kind: store.RefBranch, Target: rev.ID}); err != nil {
		return nil, err
	}
	return rev, nil
}

// gitInit creates a fresh git repo and configures the credential transport.
func gitInit(work, token string) error {
	env := remoteSetEnv(os.Environ(), token)
	step := func(args ...string) error {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = work
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	if err := step("init", "--quiet"); err != nil {
		return err
	}
	if err := step("config", "user.name", "easyvcs"); err != nil {
		return err
	}
	return step("config", "user.email", "easyvcs@example.com")
}

// clone mirrors an external branch into dir and returns the worktree.
func clone(ctx context.Context, url, branch, token, dir string) (string, error) {
	work := filepath.Join(dir, "src")
	args := []string{"clone", "--quiet", "--branch", branch, "--depth", "1", url, work}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = remoteSetEnv(os.Environ(), token)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("git clone: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return work, nil
}

// authValue produces an Authorization header value for HTTPS git transport.
//
// If the credential contains ':', it is treated as "user:password" and sent as
// Basic verbatim (works for forgejo/GitHub/GitLab and any host). A plain token
// is sent as Basic with a stable "easyvcs" username and the token as password
// (forgejo accepts a token as a password; the username is then ignored). This
// avoids the interactive username prompt on HTTPS.
func authValue(raw string) string {
	if strings.Contains(raw, ":") {
		return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(raw))
	}
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("easyvcs:"+raw))
}

// remoteSetEnv returns an env slice that injects the Authorization header via
// git's credentialed config (GIT_CONFIG_COUNT), so the secret is never placed
// on the command line or inside the remote URL.
func remoteSetEnv(base []string, token string) []string {
	if token == "" {
		return base
	}
	auth := authValue(token)
	return append(append([]string{}, base...),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0="+auth,
	)
}

// run executes a git command in dir with the optional auth env injected.
func run(ctx context.Context, dir, token string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = remoteSetEnv(os.Environ(), token)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// touch writes a marker file; reserved for future per-mirror sequencing.
func touch(dir string) error { return os.MkdirAll(dir, 0o755) }
