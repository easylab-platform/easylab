package main

import (
	"connectrpc.com/connect"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easylab-sdk-go"
)

// easylabClient bridges ext/repo onto the easylab lab API.
//
// Core repo/branch/blob/commit operations go through the typed
// easylab-sdk-go (Connect). Bookmark semantics are gone upstream — they
// map to branches. A small REST helper is retained only for tool-specific
// read-only endpoints that have no proto RPC yet (graph / compare / search),
// so the ext still works end-to-end while the contract grows.
type easylabClient struct {
	base  string
	token string
	sdk   *easylabsdk.Client
	hc    *http.Client
}

func newClient(base, token string) *easylabClient {
	if token == "" {
		token = "devtoken"
	}
	return &easylabClient{
		base:  base,
		token: token,
		sdk:   easylabsdk.New(base, token),
		hc:    &http.Client{Timeout: 30 * time.Second},
	}
}

// bookmarkInfo is a branch name + target revision id.
type bookmarkInfo struct {
	Name string
	Sha  string
}

type repoTree map[string]map[string][]bookmarkInfo

func (t repoTree) repoExists(org, repo string) bool {
	repos, ok := t[org]
	if !ok {
		return false
	}
	_, ok = repos[repo]
	return ok
}

func (t repoTree) bookmarkExists(org, repo, bookmark string) bool {
	repos, ok := t[org]
	if !ok {
		return false
	}
	for _, b := range repos[repo] {
		if b.Name == bookmark {
			return true
		}
	}
	return false
}

// GetRepoTree lists every org/repo/branch.
func (c *easylabClient) GetRepoTree(ctx context.Context) (repoTree, error) {
	repos, err := c.sdk.ListRepos(ctx)
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	tree := repoTree{}
	for _, r := range repos {
		if r.Namespace == "" || r.Name == "" {
			continue
		}
		if tree[r.Namespace] == nil {
			tree[r.Namespace] = map[string][]bookmarkInfo{}
		}
		bms, err := c.GetBookmarksDetail(ctx, r.Namespace, r.Name)
		if err != nil {
			tree[r.Namespace][r.Name] = []bookmarkInfo{}
			continue
		}
		tree[r.Namespace][r.Name] = bms
	}
	return tree, nil
}

// GetBookmarksDetail lists a repo's branches with target revision id.
func (c *easylabClient) GetBookmarksDetail(ctx context.Context, org, repo string) ([]bookmarkInfo, error) {
	bs, err := c.sdk.Branches(ctx, org, repo)
	if err != nil {
		return nil, err
	}
	out := make([]bookmarkInfo, 0, len(bs))
	for _, b := range bs {
		out = append(out, bookmarkInfo{Name: b.GetName(), Sha: b.GetSha()})
	}
	return out, nil
}

// GetBookmarks lists a repo's branch names.
func (c *easylabClient) GetBookmarks(ctx context.Context, org, repo string) ([]string, error) {
	bs, err := c.sdk.Branches(ctx, org, repo)
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		if n := b.GetName(); n != "" {
			out = append(out, n)
		}
	}
	return out, nil
}

// EnsureRepo creates org/repo (idempotent).
func (c *easylabClient) EnsureRepo(ctx context.Context, org, repo string) error {
	return c.sdk.EnsureRepo(ctx, org, repo)
}

// EnsureBookmark creates a branch at `src` unless it already exists.
func (c *easylabClient) EnsureBookmark(ctx context.Context, org, repo, src, bookmark string) error {
	tree, err := c.GetRepoTree(ctx)
	if err != nil {
		return err
	}
	if tree.bookmarkExists(org, repo, bookmark) {
		return nil
	}
	if !tree.repoExists(org, repo) {
		return errNotFound("repository %s/%s does not exist", org, repo)
	}
	return c.sdk.CreateBranch(ctx, org, repo, bookmark, src)
}

// DeleteBookmark removes the branch if it exists (idempotent).
func (c *easylabClient) DeleteBookmark(ctx context.Context, org, repo, bookmark string) error {
	tree, err := c.GetRepoTree(ctx)
	if err != nil {
		return err
	}
	if !tree.bookmarkExists(org, repo, bookmark) {
		return nil
	}
	return c.sdk.DeleteBranch(ctx, org, repo, bookmark)
}

// CanResolve reports whether the rev (branch / sha / revision) resolves.
func (c *easylabClient) CanResolve(ctx context.Context, org, repo, rev string) (bool, error) {
	bs, err := c.sdk.Branches(ctx, org, repo)
	if err != nil {
		if easylabsdk.IsNotFound(err) {
			return false, nil
		}
		return false, errDownstream("easylab", err)
	}
	for _, b := range bs {
		if b.GetName() == rev || b.GetSha() == rev {
			return true, nil
		}
	}
	revs, err := c.sdk.Revisions(ctx, org, repo, "", 200)
	if err != nil {
		return false, errDownstream("easylab", err)
	}
	for _, r := range revs {
		if r.GetRev() == rev || r.GetSha() == rev {
			return true, nil
		}
	}
	return false, nil
}

// GetBookmarkHead resolves a branch to its immutable revision id.
func (c *easylabClient) GetBookmarkHead(ctx context.Context, org, repo, bookmark string) (string, error) {
	bs, err := c.sdk.Branches(ctx, org, repo)
	if err != nil {
		return "", errDownstream("easylab", err)
	}
	for _, b := range bs {
		if b.GetName() == bookmark {
			return b.GetSha(), nil
		}
	}
	revs, err := c.sdk.Revisions(ctx, org, repo, bookmark, 1)
	if err != nil {
		return "", errDownstream("easylab", err)
	}
	if len(revs) > 0 {
		return revs[0].GetSha(), nil
	}
	return "", errDownstream("easylab", fmt.Errorf("branch %s/%s#%s not found", org, repo, bookmark))
}

// commit applies file actions as a single change on a branch. Returns a
// change view (revision/sha best-effort).
func (c *easylabClient) commit(ctx context.Context, org, repo, bookmark, message string, actions []map[string]interface{}) (map[string]interface{}, error) {
	var path, content string
	var del bool
	for _, a := range actions {
		path, _ = a["path"].(string)
		del, _ = a["delete"].(bool)
		if !del {
			data, _ := a["content_base64"].(string)
			dec, derr := base64.StdEncoding.DecodeString(data)
			if derr != nil {
				dec = []byte(data)
			}
			content = string(dec)
		}
	}
	if del {
		content = ""
	}
	_, err := c.sdk.WriteBlob(ctx, org, repo, bookmark, path, content, message)
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{"path": path, "message": message}, nil
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

// Rebase reparents a revision onto new parents via the typed SDK.
func (c *easylabClient) Rebase(ctx context.Context, org, repo, rev string, newParents []string) (string, string, error) {
	return c.sdk.Rebase(ctx, org, repo, rev, newParents)
}

// Blame returns per-line origin ids via the typed SDK.
func (c *easylabClient) Blame(ctx context.Context, org, repo, path, ref string) (map[string]interface{}, error) {
	lines, err := c.sdk.Lab.Blame(ctx, connect.NewRequest(&easylabv1.BlameRequest{Org: org, Repo: repo, Path: path, Ref: ref}))
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

// Diff returns per-file diffs for a change via the typed SDK.
func (c *easylabClient) Diff(ctx context.Context, org, repo, changeID, path string) ([]*easylabv1.DiffFile, error) {
	return c.sdk.Diff(ctx, org, repo, changeID, path)
}
