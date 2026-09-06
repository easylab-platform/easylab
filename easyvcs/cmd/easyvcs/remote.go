package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"

	"easyvcs/internal/object"
	"easyvcs/internal/revision"
	"easyvcs/internal/store"
	"easyvcs/internal/transfer"
)

// advertiseReq/Resp mirrors the server's smart protocol request/response.
type advertiseReq struct {
	Have        []string `json:"have"`
	HaveObjects []string `json:"have_objects"`
	WantChanges []string `json:"want_changes,omitempty"`
}

type advertiseResp struct {
	Repo    string       `json:"repo"`
	Changes []string     `json:"changes"`
	Refs    []*store.Ref `json:"refs"`
}

// --- remote configuration ---

func cmdRemote(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "remote:", err)
		os.Exit(1)
	}
	args := os.Args[2:]
	if len(args) == 0 {
		remotes, err := repo.ListRemotes()
		if err != nil {
			fmt.Fprintln(os.Stderr, "remote:", err)
			os.Exit(1)
		}
		for _, r := range remotes {
			fmt.Printf("%s\t%s\n", r.Name, r.URL)
		}
		return
	}
	switch args[0] {
	case "add":
		token := ""
		url := ""
		var name string
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--token", "-token":
				if i+1 < len(args) {
					token = args[i+1]
					i++
				}
			case "--url", "-url":
				if i+1 < len(args) {
					url = args[i+1]
					i++
				}
			default:
				if name == "" {
					name = args[i]
				} else if url == "" {
					url = args[i]
				}
			}
		}
		if name == "" || url == "" {
			fmt.Fprintln(os.Stderr, "usage: remote add <name> <url> [--token <token>]")
			os.Exit(1)
		}
		if err := repo.PutRemote(name, url, token); err != nil {
			fmt.Fprintln(os.Stderr, "remote add:", err)
			os.Exit(1)
		}
		if token != "" {
			fmt.Printf("added remote %s -> %s (with token)\n", name, url)
		} else {
			fmt.Printf("added remote %s -> %s\n", name, url)
		}
	case "remove", "rm":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "usage: remote remove <name>")
			os.Exit(1)
		}
		if err := repo.DeleteRemote(args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "remote remove:", err)
			os.Exit(1)
		}
		fmt.Printf("removed remote %s\n", args[1])
	default:
		rem, err := repo.GetRemote(args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, "remote:", err)
			os.Exit(1)
		}
		fmt.Printf("%s\t%s\n", rem.Name, rem.URL)
	}
}

// --- smart protocol: pull / push ---

// doPost performs an HTTP POST with an optional gzip-compressed body and token,
// returning the raw response body bytes.
func doPost(url string, body []byte, token string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if len(body) > 1024 {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write(body); err == nil {
			if err := gw.Close(); err == nil {
				req.Header.Set("Content-Encoding", "gzip")
				req.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
				req.ContentLength = int64(buf.Len())
			}
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server error %d: %s", resp.StatusCode, string(rb))
	}
	return io.ReadAll(resp.Body)
}

// postJSON is a thin wrapper around doPost for small JSON payloads with a JSON
// response.
func postJSON(url string, body any, out any, token string) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if len(payload) > 1024 {
		var buf bytes.Buffer
		gw := gzip.NewWriter(&buf)
		if _, err := gw.Write(payload); err == nil {
			if err := gw.Close(); err == nil {
				req.Header.Set("Content-Encoding", "gzip")
				req.Body = io.NopCloser(bytes.NewReader(buf.Bytes()))
				req.ContentLength = int64(buf.Len())
			}
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("server error %d: %s", resp.StatusCode, string(rb))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// postBundle sends a fully-encoded bundle frame to the server and returns the
// raw response bytes.
//
// CompressBundle already produces a gzip-compressed binary frame, so the bytes
// are sent verbatim with Content-Encoding: gzip — doPost's size-based gzip must
// NOT be applied on top, otherwise the server sees a second gzip layer and
// cannot decode the frame.
func postBundle(url string, bundle *transfer.Bundle, token string) ([]byte, error) {
	if encoded, err := transfer.CompressBundle(bundle); err == nil {
		return doPostCompressed(url, encoded, token)
	}
	// Fallback to JSON if binary encoding fails.
	payload, _ := json.Marshal(bundle)
	return doPost(url, payload, token)
}

// doPostCompressed sends a body that is already gzip-compressed by the caller,
// marking it with Content-Encoding: gzip so the server decompresses exactly
// once. It never re-compresses the body.
func doPostCompressed(url string, body []byte, token string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "gzip")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("server error %d: %s", resp.StatusCode, string(rb))
	}
	return io.ReadAll(resp.Body)
}

// getBundle decodes either a binary bundle or a JSON bundle from raw bytes.
func getBundle(data []byte) (*transfer.Bundle, error) {
	if len(data) > 0 && data[0] != '{' {
		return transfer.UnmarshalBinary(data)
	}
	var b transfer.Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

func resolveRemote(repo *store.Repo, nameOrURL string) (*store.Remote, error) {
	if rem, err := repo.GetRemote(nameOrURL); err == nil {
		return rem, nil
	}
	return &store.Remote{Name: nameOrURL, URL: nameOrURL}, nil
}

func trimTrailingSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func currentChangeIDs(repo *store.Repo) []string {
	ch, err := repo.ListRevisions()
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(ch))
	for _, c := range ch {
		ids = append(ids, c.ID)
	}
	return ids
}

// baseForRepo joins a remote base URL with the repo's namespace/name path used
// by the smart protocol endpoints (/repo/{ns}/{name}/...).
func baseForRepo(base string, repoRef store.RepoRef) string {
	base = trimTrailingSlash(base)
	return base + "/repo/" + repoRef.Namespace + "/" + repoRef.Name
}

// cmdFetch fetches remote branchs (all, or those named in args) and records
// them into the local remote_refs namespace (git's refs/remotes).
func cmdFetch(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
	if err := doFetch(repo, os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "fetch:", err)
		os.Exit(1)
	}
}

// doFetch performs the fetch against repo directly (no workspace dependency).
func doFetch(repo *store.Repo, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: fetch <remote> [ref...]")
	}
	rem, err := resolveRemote(repo, args[0])
	if err != nil {
		return err
	}
	only := args[1:]
	full := baseForRepo(rem.URL, repo.RepoRef())

	var adv advertiseResp
	if err := postJSON(full+"/advertise", advertiseReq{Have: currentChangeIDs(repo)}, &adv, ""); err != nil {
		return err
	}

	// Determine which refs to record. With no explicit refs, record all branchs.
	list := adv.Refs
	if len(only) > 0 {
		var sel []*store.Ref
		for _, r := range adv.Refs {
			for _, o := range only {
				if r.Name == o {
					sel = append(sel, r)
				}
			}
		}
		list = sel
	}

	// Record each into remote_refs and remember the default branch.
	count := 0
	for _, r := range list {
		if r.Kind != store.RefBranch {
			continue
		}
		if err := repo.SetRemoteRef(rem.Name, &store.RemoteRef{RemoteName: rem.Name, Kind: store.RefBranch, Name: r.Name, Target: r.Target}); err != nil {
			return err
		}
		count++
	}
	// Record the remote's default branch if adv exposes it (advertise does not
	// carry it yet; fall back to the first branch).
	if def, _ := repo.GetRemoteDefaultBranch(rem.Name); def == "" && len(list) > 0 {
		if err := repo.SetRemoteDefaultBranch(rem.Name, list[0].Name); err != nil {
			return err
		}
	}
	fmt.Printf("fetched %d ref(s) from %s\n", count, rem.URL)
	return nil
}

func cmdPull(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pull:", err)
		os.Exit(1)
	}
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: pull <remote> [branch]")
		os.Exit(1)
	}
	rem, err := resolveRemote(repo, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "pull:", err)
		os.Exit(1)
	}
	// Determine the branch to merge: explicit arg, else the remote's default
	// branch, else the repo's default branch.
	want := ""
	if len(args) > 1 {
		want = args[1]
	} else if def, _ := repo.GetRemoteDefaultBranch(rem.Name); def != "" {
		want = def
	} else if meta, _ := repo.RepoMeta(); meta.DefaultBranch != "" {
		want = meta.DefaultBranch
	}
	if want == "" {
		fmt.Fprintln(os.Stderr, "pull: no branch specified and no default; pass a branch name")
		os.Exit(1)
	}
	full := baseForRepo(rem.URL, repo.RepoRef())

	var adv advertiseResp
	if err := postJSON(full+"/advertise", advertiseReq{Have: currentChangeIDs(repo)}, &adv, ""); err != nil {
		fmt.Fprintln(os.Stderr, "pull:", err)
		os.Exit(1)
	}

	// Restrict the fetch to just that chain, and capture the remote tip.
	var fetchReq advertiseReq = advertiseReq{
		Have:        currentChangeIDs(repo),
		HaveObjects: objectIDsAsStrings(ownObjects(repo)),
	}
	remoteTip := ""
	found := false
	for _, r := range adv.Refs {
		if r.Name == want && r.Kind == store.RefBranch {
			found = true
			fetchReq.WantChanges = []string{r.Target}
			remoteTip = r.Target
			break
		}
	}
	if !found {
		fmt.Fprintln(os.Stderr, "pull: branch not found on server:", want)
		os.Exit(1)
	}
	// Collaborative pull: fetch objects, then rebase the remote tip onto the
	// local branch tip so the two merge into one line.
	if err := collaborativePull(c, repo, rem, full, fetchReq, want, remoteTip); err != nil {
		fmt.Fprintln(os.Stderr, "pull:", err)
		os.Exit(1)
	}
}

// doPull performs pull directly against repo (no workspace dependency).
func doPull(repo *store.Repo, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: pull <remote> [branch]")
	}
	rem, err := resolveRemote(repo, args[0])
	if err != nil {
		return err
	}
	want := ""
	if len(args) > 1 {
		want = args[1]
	} else if def, _ := repo.GetRemoteDefaultBranch(rem.Name); def != "" {
		want = def
	} else if meta, _ := repo.RepoMeta(); meta.DefaultBranch != "" {
		want = meta.DefaultBranch
	}
	if want == "" {
		return fmt.Errorf("no branch specified and no default; pass a branch name")
	}
	full := baseForRepo(rem.URL, repo.RepoRef())

	var adv advertiseResp
	if err := postJSON(full+"/advertise", advertiseReq{Have: currentChangeIDs(repo)}, &adv, ""); err != nil {
		return err
	}
	var fetchReq advertiseReq = advertiseReq{
		Have:        currentChangeIDs(repo),
		HaveObjects: objectIDsAsStrings(ownObjects(repo)),
	}
	remoteTip := ""
	found := false
	for _, r := range adv.Refs {
		if r.Name == want && r.Kind == store.RefBranch {
			found = true
			fetchReq.WantChanges = []string{r.Target}
			remoteTip = r.Target
			break
		}
	}
	if !found {
		return fmt.Errorf("branch not found on server: %s", want)
	}
	return collaborativePull(&ctx{}, repo, rem, full, fetchReq, want, remoteTip)
}

// collaborativePull fetches a remote branch's objects and then rebases the
// remote tip revision onto the local branch tip, so the two edits merge into
// one linear change (conflicts become first-class objects). It re-points the
// local branch to the merged result and records the remote tip for the next
// incremental pull.
func collaborativePull(c *ctx, repo *store.Repo, rem *store.Remote, full string, fetchReq advertiseReq, branch, remoteTip string) error {
	// Ensure the local branch exists (create it if missing).
	ws := revision.NewWorkspace(repo)
	localRef, err := ws.GetRef(branch)
	if err != nil || localRef == nil {
		localRef = &store.Ref{Name: branch, Kind: store.RefBranch}
	}

	// Fetch objects (they may not be present yet).
	if err := postJSON(full+"/fetch", fetchReq, nil, ""); err != nil {
		return err
	}
	data, err := fetchRaw(full, fetchReq, "")
	if err != nil {
		return err
	}
	b, err := getBundle(data)
	if err != nil {
		return err
	}
	if _, err := transfer.Apply(repo, b); err != nil {
		return err
	}

	// The remote tip's revision id (transferred object). Resolve to its current
	// snapshot hash within this store.
	remoteRev, err := repo.GetRevision(remoteTip)
	if err != nil {
		// Could not resolve the remote tip id; fall back to a plain apply.
		return err
	}
	_ = remoteRev // snapshot hash not needed here; rebase resolves it internally.

	// Determine the local tip to rebase onto.
	localTip := localRef.Target
	if localTip == "" {
		// No local tip yet: place the remote change directly as the local tip.
		if _, err := ws.SetRef(branch, store.RefBranch, remoteTip); err != nil {
			return err
		}
		if err := repo.UpdateRemoteSyncTip(rem.Name, remoteTip); err != nil {
			return err
		}
		fmt.Printf("pulled %s -> %s (no local tip; placed remote tip)\n", rem.URL, short(remoteTip))
		return nil
	}

	// The local tip's snapshot hash; the remote tip is rebased ONTO it so the
	// remote edit merges into the local line (conflicts -> first-class objects).
	localRev, err := repo.GetRevision(localTip)
	if err != nil {
		return err
	}
	localHash := localRev.Hash

	// Rebase the remote tip onto the local tip's snapshot (3-way).
	if _, _, err := ws.Rebase(remoteTip, []object.ID{localHash}); err != nil {
		return err
	}

	// Re-point the local branch to the merged (rebased) remote revision id.
	mergedRev, err := repo.GetRevision(remoteTip)
	if err != nil {
		return err
	}
	if _, err := ws.SetRef(branch, store.RefBranch, mergedRev.ID); err != nil {
		return err
	}
	if err := repo.UpdateRemoteSyncTip(rem.Name, mergedRev.ID); err != nil {
		return err
	}
	snap, _ := repo.GetSnapshot(mergedRev.Hash)
	if snap != nil {
		fmt.Printf("pulled %s -> %s merged onto %s (revision %s)\n", rem.URL, short(remoteTip), short(localTip), short(mergedRev.ID))
	}
	return nil
}

// fetchRaw performs a fetch request and returns raw bytes (binary bundle).
func fetchRaw(full string, req advertiseReq, token string) ([]byte, error) {
	payload, _ := json.Marshal(req)
	return doPost(full+"/fetch", payload, token)
}

func cmdPush(c *ctx) {
	repo, _, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}
	args := os.Args[2:]
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: push <remote>")
		os.Exit(1)
	}
	rem, err := resolveRemote(repo, args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}
	full := baseForRepo(rem.URL, repo.RepoRef())

	b, err := transfer.CollectAll(repo)
	if err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}
	data, err := postBundle(full+"/push", b, rem.Token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}
	var resp map[string]any
	if err := json.Unmarshal(data, &resp); err != nil {
		fmt.Fprintln(os.Stderr, "push:", err)
		os.Exit(1)
	}
	fmt.Printf("pushed to %s: %v\n", rem.URL, resp)
}

// ownObjects returns all object ids the repo already holds.
func ownObjects(repo *store.Repo) []object.ID {
	ids, _ := transfer.EnumerateObjectIDs(repo)
	return ids
}

// objectIDsAsStrings converts object ids to hex strings.
func objectIDsAsStrings(ids []object.ID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// --- git interop (no smart protocol, full transfer + minimal diff commit) ---

func cmdGitPull(c *ctx) {
	gitURLOrDir := gitArg()
	if gitURLOrDir == "" {
		fmt.Fprintln(os.Stderr, "usage: git-pull <git-repo-or-url> [DIR]")
		os.Exit(1)
	}
	repo, marker, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-pull:", err)
		os.Exit(1)
	}
	ws := revision.NewWorkspace(repo)

	tmp, err := os.MkdirTemp("", "easyvcs-gitpull-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-pull:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)
	cmd := exec.Command("git", "clone", "--quiet", gitURLOrDir, tmp)
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "git-pull: git clone failed:", err)
		os.Exit(1)
	}

	parentSnap, _ := ws.ParentsOfChange(marker.CurrentRevision)
	if len(parentSnap) == 0 {
		parentSnap = repoTip(repo)
	}
	treeID, err := ws.BuildTreeFromFS(tmp)
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-pull:", err)
		os.Exit(1)
	}
	// Record the incoming git tree as a single change. If the workspace has a
	// current change, amend it (finalize that change to the incoming content);
	// otherwise create a new change linked to the tip.
	changeID := marker.CurrentRevision
	snap, ch, err := ws.Commit(revision.CommitParams{
		RevisionID:  changeID,
		Parents:     parentSnap,
		TreeID:      treeID,
		Description: "pull from git: " + gitURLOrDir,
		Author:      store.Author{Name: "easyvcs", Email: "easyvcs@example.com"},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-pull:", err)
		os.Exit(1)
	}
	marker.CurrentRevision = ch.ID
	_ = store.WriteMarker(".", marker)
	_ = c.cs.PutWorkspace(&store.WorkspaceRow{Path: ".", Repo: marker.Repo, CurrentRevision: ch.ID, Branch: marker.Branch})
	fmt.Printf("pulled from git as change %s -> snapshot %s\n", short(ch.ID), short(snap.RevisionHash.String()))
}

func cmdGitPush(c *ctx) {
	gitDest := gitArg()
	if gitDest == "" {
		fmt.Fprintln(os.Stderr, "usage: git-push <git-repo-or-url> [BRANCH]")
		os.Exit(1)
	}
	repo, marker, err := c.loadRepo()
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-push:", err)
		os.Exit(1)
	}
	ws := revision.NewWorkspace(repo)

	tmp, err := os.MkdirTemp("", "easyvcs-gitpush-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-push:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)
	cur, err := repo.GetRevision(marker.CurrentRevision)
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-push: no current change:", err)
		os.Exit(1)
	}
	snap, err := repo.GetSnapshot(cur.Hash)
	if err != nil {
		fmt.Fprintln(os.Stderr, "git-push:", err)
		os.Exit(1)
	}
	if err := ws.Materialize(snap.TreeID, tmp); err != nil {
		fmt.Fprintln(os.Stderr, "git-push:", err)
		os.Exit(1)
	}
	msg := snap.Description
	if msg == "" {
		msg = "easyvcs push"
	}

	work := filepath.Join(os.TempDir(), "easyvcs-gitwork")
	_ = os.RemoveAll(work)
	cmd := exec.Command("git", "clone", "--quiet", gitDest, work)
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "git-push: could not clone dest:", err)
		os.Exit(1)
	}
	// Only copy the materialized tree contents (exclude the temp metadata dir
	// marker), into the clone's working tree.
	if err := copyDirTree(tmp, work, true); err != nil {
		fmt.Fprintln(os.Stderr, "git-push:", err)
		os.Exit(1)
	}
	cmd = exec.Command("git", "-C", work, "add", "-A")
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "git-push:", err)
		os.Exit(1)
	}
	cmd = exec.Command("git", "-C", work, "commit", "--quiet", "-m", msg, "--allow-empty")
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "git-push:", err)
		os.Exit(1)
	}
	cmd = exec.Command("git", "-C", work, "push", "--quiet")
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "git-push:", err)
		os.Exit(1)
	}
	fmt.Printf("pushed current change to git %s\n", gitDest)
}

func gitArg() string {
	args := os.Args[2:]
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

func copyDirTree(src, dst string, skipGit bool) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if skipGit && (e.Name() == ".git" || e.Name() == ".easyvcs") {
			continue
		}
		sPath := filepath.Join(src, e.Name())
		dPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := os.MkdirAll(dPath, 0o755); err != nil {
				return err
			}
			if err := copyDirTree(sPath, dPath, skipGit); err != nil {
				return err
			}
			continue
		}
		if err := copyFile(sPath, dPath); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, 0o644)
}
