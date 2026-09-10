package repoext

import (
	"bytes"
	"connectrpc.com/connect"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	easylabclient "github.com/easylab-platform/easylab/internal/easylabclient"
)

// easylabClient bridges ext/repo onto the easylab lab API.
//
// Core repo/branch/blob/commit operations go through the generated easylab
// Connect client. Branch semantics are gone upstream — they map to branches.
// A small REST helper is retained only for tool-specific read-only endpoints
// that have no proto RPC yet, so the ext still works end-to-end while the
// contract grows.
type easylabClient struct {
	base  string
	token string
	svc   *easylabclient.Services
	hc    *http.Client
}

func newClient(base, token string) *easylabClient {
	if token == "" {
		token = "devtoken"
	}
	return &easylabClient{
		base:  base,
		token: token,
		svc:   easylabclient.New(base, token),
		hc:    &http.Client{Timeout: 30 * time.Second},
	}
}

// branchInfo is a branch name + target revision id.
type branchInfo struct {
	Name string
	Sha  string
}

type repoTree map[string]map[string][]branchInfo

func (t repoTree) repoExists(org, repo string) bool {
	repos, ok := t[org]
	if !ok {
		return false
	}
	_, ok = repos[repo]
	return ok
}

func (t repoTree) branchExists(org, repo, branch string) bool {
	repos, ok := t[org]
	if !ok {
		return false
	}
	for _, b := range repos[repo] {
		if b.Name == branch {
			return true
		}
	}
	return false
}

// GetRepoTree lists every org/repo/branch.
func (c *easylabClient) GetRepoTree(ctx context.Context) (repoTree, error) {
	res, err := c.svc.Lab.ListRepos(ctx, connect.NewRequest(&easylabv1.ListReposRequest{}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	tree := repoTree{}
	for _, r := range res.Msg.GetRepos() {
		if r.Namespace == "" || r.Name == "" {
			continue
		}
		if tree[r.Namespace] == nil {
			tree[r.Namespace] = map[string][]branchInfo{}
		}
		bms, err := c.GetBranchesDetail(ctx, r.Namespace, r.Name)
		if err != nil {
			tree[r.Namespace][r.Name] = []branchInfo{}
			continue
		}
		tree[r.Namespace][r.Name] = bms
	}
	return tree, nil
}

// GetBranchesDetail lists a repo's branches with target revision id.
func (c *easylabClient) GetBranchesDetail(ctx context.Context, org, repo string) ([]branchInfo, error) {
	res, err := c.svc.Lab.Branches(ctx, connect.NewRequest(&easylabv1.BranchesRequest{Org: org, Repo: repo}))
	if err != nil {
		return nil, err
	}
	out := make([]branchInfo, 0, len(res.Msg.GetBranches()))
	for _, b := range res.Msg.GetBranches() {
		out = append(out, branchInfo{Name: b.GetName(), Sha: b.GetSha()})
	}
	return out, nil
}

// GetBranches lists a repo's branch names.
func (c *easylabClient) GetBranches(ctx context.Context, org, repo string) ([]string, error) {
	res, err := c.svc.Lab.Branches(ctx, connect.NewRequest(&easylabv1.BranchesRequest{Org: org, Repo: repo}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	out := make([]string, 0, len(res.Msg.GetBranches()))
	for _, b := range res.Msg.GetBranches() {
		if n := b.GetName(); n != "" {
			out = append(out, n)
		}
	}
	return out, nil
}

// EnsureRepo creates org/repo (idempotent).
func (c *easylabClient) EnsureRepo(ctx context.Context, org, repo string) error {
	_, err := c.svc.Lab.EnsureRepo(ctx, connect.NewRequest(&easylabv1.EnsureRepoRequest{Org: org, Repo: repo}))
	if err == nil || connect.CodeOf(err) == connect.CodeAlreadyExists || connect.CodeOf(err) == connect.CodeInvalidArgument {
		return nil
	}
	return errDownstream("easylab", err)
}

// EnsureBranch creates a branch at `src` unless it already exists.
func (c *easylabClient) EnsureBranch(ctx context.Context, org, repo, src, branch string) error {
	tree, err := c.GetRepoTree(ctx)
	if err != nil {
		return err
	}
	if tree.branchExists(org, repo, branch) {
		return nil
	}
	if !tree.repoExists(org, repo) {
		return errNotFound("repository %s/%s does not exist", org, repo)
	}
	_, err = c.svc.Lab.CreateBranch(ctx, connect.NewRequest(&easylabv1.CreateBranchRequest{
		Org: org, Repo: repo, Branch: branch, From: src,
	}))
	if err != nil && connect.CodeOf(err) != connect.CodeAlreadyExists && connect.CodeOf(err) != connect.CodeInvalidArgument {
		return errDownstream("easylab", err)
	}
	return nil
}

// DeleteBranch removes the branch if it exists (idempotent).
func (c *easylabClient) DeleteBranch(ctx context.Context, org, repo, branch string) error {
	tree, err := c.GetRepoTree(ctx)
	if err != nil {
		return err
	}
	if !tree.branchExists(org, repo, branch) {
		return nil
	}
	_, err = c.svc.Lab.DeleteBranch(ctx, connect.NewRequest(&easylabv1.DeleteBranchRequest{
		Org: org, Repo: repo, Branch: branch,
	}))
	if err != nil && connect.CodeOf(err) != connect.CodeNotFound {
		return errDownstream("easylab", err)
	}
	return nil
}

// CanResolve reports whether the rev (branch / sha / revision) resolves.
func (c *easylabClient) CanResolve(ctx context.Context, org, repo, rev string) (bool, error) {
	bsRes, err := c.svc.Lab.Branches(ctx, connect.NewRequest(&easylabv1.BranchesRequest{Org: org, Repo: repo}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return false, nil
		}
		return false, errDownstream("easylab", err)
	}
	for _, b := range bsRes.Msg.GetBranches() {
		if b.GetName() == rev || b.GetSha() == rev {
			return true, nil
		}
	}
	revsRes, err := c.svc.Lab.Revisions(ctx, connect.NewRequest(&easylabv1.RevisionsRequest{
		Org: org, Repo: repo, Limit: 200,
	}))
	if err != nil {
		return false, errDownstream("easylab", err)
	}
	for _, r := range revsRes.Msg.GetRevisions() {
		if r.GetRev() == rev || r.GetSha() == rev {
			return true, nil
		}
	}
	return false, nil
}

// GetBranchHead resolves a branch to its head SNAPSHOT hash (32-byte object
// id, 64 hex chars) — the form rebase/compare APIs take as parents. A branch
// ref's stored target is often a 16-byte revision id, which those APIs reject,
// so the revision log (CommitId = snapshot hash) is authoritative.
func (c *easylabClient) GetBranchHead(ctx context.Context, org, repo, branch string) (string, error) {
	revsRes, rerr := c.svc.Lab.Revisions(ctx, connect.NewRequest(&easylabv1.RevisionsRequest{
		Org: org, Repo: repo, Ref: branch, Limit: 1,
	}))
	if rerr == nil && len(revsRes.Msg.GetRevisions()) > 0 {
		if sha := revsRes.Msg.GetRevisions()[0].GetSha(); sha != "" {
			return sha, nil
		}
	}
	bsRes, err := c.svc.Lab.Branches(ctx, connect.NewRequest(&easylabv1.BranchesRequest{Org: org, Repo: repo}))
	if err != nil {
		return "", errDownstream("easylab", err)
	}
	for _, b := range bsRes.Msg.GetBranches() {
		if b.GetName() == branch {
			// Only trust the ref target when it is a full snapshot hash.
			if sha := b.GetSha(); len(sha) == 64 {
				return sha, nil
			}
			break
		}
	}
	return "", errDownstream("easylab", fmt.Errorf("branch %s/%s#%s not found", org, repo, branch))
}

// commit applies file actions as a single change on a branch. Returns a
// change view (revision/sha best-effort).
func (c *easylabClient) commit(ctx context.Context, org, repo, branch, message string, actions []map[string]interface{}) (map[string]interface{}, error) {
	// Atomic multi-action commit over the REST commit endpoint: unlike the
	// single-blob WriteBlob RPC it carries real delete semantics (writing an
	// empty blob instead of deleting would poison sandbox sync, whose object
	// decoder cannot distinguish an empty blob from a missing one).
	changes := make([]map[string]interface{}, 0, len(actions))
	for _, a := range actions {
		path, _ := a["path"].(string)
		del, _ := a["delete"].(bool)
		if act, _ := a["action"].(string); act == "delete" {
			del = true
		}
		if del {
			changes = append(changes, map[string]interface{}{"path": path, "delete": true})
			continue
		}
		data, _ := a["content_base64"].(string)
		dec, derr := base64.StdEncoding.DecodeString(data)
		if derr != nil {
			dec = []byte(data)
		}
		changes = append(changes, map[string]interface{}{"path": path, "content": string(dec)})
	}
	return c.post(ctx, fmt.Sprintf("/repo/%s/%s/commit", url.PathEscape(org), url.PathEscape(repo)), map[string]interface{}{
		"ref":         branch,
		"description": message,
		"new_commit":  true,
		"changes":     changes,
	})
}

// get/post/put/delete are retained for the few tool-specific endpoints that
// have no proto RPC yet (graph / compare / search / rebase). They are the
// thin REST facade; all repo/branch/blob operations above use the SDK.
func (c *easylabClient) get(ctx context.Context, path string) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, errNotFoundForHTTP
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return wrapJSON(raw), nil
}

func (c *easylabClient) post(ctx context.Context, path string, body interface{}) (map[string]interface{}, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	defer resp.Body.Close()
	var v map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return v, nil
}

func (c *easylabClient) put(ctx context.Context, path string, body interface{}) (map[string]interface{}, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	defer resp.Body.Close()
	var v map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return v, nil
}

func (c *easylabClient) delete(ctx context.Context, path string, body interface{}) (map[string]interface{}, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	defer resp.Body.Close()
	var v map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return v, nil
}

// wrapJSON decodes a response; a top-level array is placed under "_arr".
func wrapJSON(raw json.RawMessage) map[string]interface{} {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return map[string]interface{}{}
	}
	if strings.HasPrefix(trimmed, "[") {
		var arr []interface{}
		_ = json.Unmarshal(raw, &arr)
		return map[string]interface{}{"_arr": arr}
	}
	var m map[string]interface{}
	_ = json.Unmarshal(raw, &m)
	return m
}

func errText(v map[string]interface{}) string {
	if v == nil {
		return ""
	}
	if s, ok := v["error"].(string); ok {
		return s
	}
	if s, ok := v["message"].(string); ok {
		return s
	}
	return ""
}

var _ = easylabv1.BranchInfo{}

// Rebase reparents a revision onto new parents via the generated client.
func (c *easylabClient) Rebase(ctx context.Context, org, repo, rev string, newParents []string) (string, string, error) {
	res, err := c.svc.Lab.Rebase(ctx, connect.NewRequest(&easylabv1.RebaseRequest{
		Org: org, Repo: repo, Rev: rev, NewParents: newParents,
	}))
	if err != nil {
		return "", "", errDownstream("easylab", err)
	}
	return res.Msg.GetRevisionId(), res.Msg.GetSnapshot(), nil
}

// Blame returns per-line origin ids via the generated client.
func (c *easylabClient) Blame(ctx context.Context, org, repo, path, ref string) (map[string]interface{}, error) {
	lines, err := c.svc.Lab.Blame(ctx, connect.NewRequest(&easylabv1.BlameRequest{Org: org, Repo: repo, Path: path, Ref: ref}))
	if err != nil {
		return nil, err
	}
	out := map[string]interface{}{"_arr": []interface{}{}}
	arr := make([]interface{}, 0, len(lines.Msg.GetLines()))
	for i, l := range lines.Msg.GetLines() {
		arr = append(arr, map[string]interface{}{"revision_id": l, "line_number": i + 1, "content": ""})
	}
	out["_arr"] = arr
	return out, nil
}

// Diff returns per-file diffs for a change via the generated client.
func (c *easylabClient) Diff(ctx context.Context, org, repo, changeID, path string) ([]*easylabv1.DiffFile, error) {
	res, err := c.svc.Lab.Diff(ctx, connect.NewRequest(&easylabv1.DiffRequest{Org: org, Repo: repo, ChangeId: changeID, Path: path}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetFiles(), nil
}
