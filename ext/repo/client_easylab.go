package main

import (
	"bytes"
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
)

// easylabClient is a thin, status-aware client for the easylab REST API.
//
// Read endpoints are anonymous; every mutation requires a write token, sent
// as `Authorization: Bearer <token>`. easylab serves revision-native repos
// under /api/v1/repo, bookmarks under /bookmarks, trees under /tree?ref=.
type easylabClient struct {
	base  string
	token string
	hc    *http.Client
}

func newClient(base, token string) *easylabClient {
	if token == "" {
		token = "devtoken"
	}
	return &easylabClient{base: base, token: token, hc: &http.Client{Timeout: 30 * time.Second}}
}

func (c *easylabClient) call(ctx context.Context, method, path string, body interface{}) (int, map[string]interface{}, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	var v map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&v)
	return resp.StatusCode, v, nil
}

// repoTree is the read-only snapshot of org → repo → bookmarks.
type bookmarkInfo struct {
	Name string
	Sha  string
}

type repoTree map[string]map[string][]bookmarkInfo

// GetRepoTree lists every org/repo/bookmark. easylab's GET /api/v1/repo returns
// a bare JSON array; we fan out one GET /bookmarks per repo.
func (c *easylabClient) GetRepoTree(ctx context.Context) (repoTree, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/api/v1/repo", nil)
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errDownstream("easylab", fmt.Errorf("GET /repo: HTTP %d", resp.StatusCode))
	}
	var arr []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&arr); err != nil {
		return nil, errDownstream("easylab", err)
	}
	tree := repoTree{}
	for _, rm := range arr {
		org, _ := rm["namespace"].(string)
		repo, _ := rm["name"].(string)
		if org == "" || repo == "" {
			continue
		}
		if tree[org] == nil {
			tree[org] = map[string][]bookmarkInfo{}
		}
		bms, err := c.GetBookmarksDetail(ctx, org, repo)
		if err != nil {
			tree[org][repo] = []bookmarkInfo{}
			continue
		}
		tree[org][repo] = bms
	}
	return tree, nil
}

// toBookmarks converts an easylab /bookmarks response into bookmarkInfo.
func toBookmarks(v map[string]interface{}) []bookmarkInfo {
	arr, ok := v["_arr"].([]interface{})
	if !ok {
		arr, _ = v["bookmarks"].([]interface{})
	}
	out := make([]bookmarkInfo, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, bookmarkInfo{Name: strFrom(m, "name"), Sha: strFrom(m, "revision_id")})
	}
	return out
}

// GetBookmarksDetail lists a repo's bookmarks with their target revision id.
func (c *easylabClient) GetBookmarksDetail(ctx context.Context, org, repo string) ([]bookmarkInfo, error) {
	v, err := c.get(ctx, "/api/v1/repo/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/bookmarks")
	if err != nil {
		return nil, err
	}
	return toBookmarks(v), nil
}

// GetBookmarks lists a repo's bookmark names.
func (c *easylabClient) GetBookmarks(ctx context.Context, org, repo string) ([]string, error) {
	status, v, err := c.call(ctx, http.MethodGet,
		"/api/v1/repo/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/bookmarks", nil)
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	if status != 200 {
		return nil, errDownstream("easylab", fmt.Errorf("GET bookmarks: HTTP %d", status))
	}
	var out []string
	for _, be := range sliceOf(v["bookmarks"]) {
		if bm, ok := be.(map[string]interface{}); ok {
			if n, _ := bm["name"].(string); n != "" {
				out = append(out, n)
			}
		}
	}
	return out, nil
}

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

// EnsureRepo creates org/repo (idempotent: a 409 "already exists" is success).
func (c *easylabClient) EnsureRepo(ctx context.Context, org, repo string) error {
	status, v, err := c.call(ctx, http.MethodPost, "/api/v1/repo",
		map[string]interface{}{"namespace": org, "name": repo})
	if err != nil {
		return errDownstream("easylab", err)
	}
	switch status {
	case 200, 201, 409:
		return nil
	default:
		return errDownstream("easylab", fmt.Errorf("ensure repo: HTTP %d %s", status, errText(v)))
	}
}

// EnsureBookmark creates a bookmark at `src` (a revision ref), unless it
// already exists (idempotent forward step).
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
	status, v, err := c.call(ctx, http.MethodPost,
		"/api/v1/repo/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/bookmarks",
		map[string]interface{}{"name": bookmark, "target": src})
	if err != nil {
		return errDownstream("easylab", err)
	}
	if status != 200 && status != 201 {
		return errDownstream("easylab", fmt.Errorf("create bookmark %q: HTTP %d %s", src, status, errText(v)))
	}
	return nil
}

// DeleteBookmark removes the bookmark if it exists (idempotent).
func (c *easylabClient) DeleteBookmark(ctx context.Context, org, repo, bookmark string) error {
	tree, err := c.GetRepoTree(ctx)
	if err != nil {
		return err
	}
	if !tree.bookmarkExists(org, repo, bookmark) {
		return nil
	}
	status, v, err := c.call(ctx, http.MethodDelete,
		"/api/v1/repo/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/bookmarks/"+url.PathEscape(bookmark), nil)
	if err != nil {
		return errDownstream("easylab", err)
	}
	if status != 200 && status != 204 {
		return errDownstream("easylab", fmt.Errorf("delete bookmark: HTTP %d %s", status, errText(v)))
	}
	return nil
}

// CanResolve reports whether the rev (bookmark / sha / tag / revision id)
// resolves in the repo. easylab resolves via GET /tree?ref=.
func (c *easylabClient) CanResolve(ctx context.Context, org, repo, rev string) (bool, error) {
	status, _, err := c.call(ctx, http.MethodGet,
		"/api/v1/repo/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/bookmarks", nil)
	if err != nil {
		return false, errDownstream("easylab", err)
	}
	switch status {
	case 200:
		return true, nil
	case 404:
		return false, nil
	default:
		return false, errDownstream("easylab", fmt.Errorf("resolve %q: HTTP %d", rev, status))
	}
}

func sliceOf(v interface{}) []interface{} {
	if arr, ok := v.([]interface{}); ok {
		return arr
	}
	return nil
}

// get/post/put/delete are the token-authenticated JSON round-trips used by the
// tool handlers. They inherit the shared httpx contract (404 → ErrNotFound),
// while injecting the write token on every request.
func (c *easylabClient) get(ctx context.Context, path string) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	return c.doJSON(req)
}

func (c *easylabClient) post(ctx context.Context, path string, body interface{}) (map[string]interface{}, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.doJSON(req)
}

// commit applies one or more file actions (create/update/delete) as a single
// change on easylab via POST /files (default amends onto the bookmark tip).
// Actions use content_base64 like the Gitea DSL; we decode to plain content.
func (c *easylabClient) commit(ctx context.Context, org, repo, bookmark, message string, actions []map[string]interface{}) (map[string]interface{}, error) {
	changes := make([]interface{}, 0, len(actions))
	for _, a := range actions {
		act, _ := a["action"].(string)
		path, _ := a["path"].(string)
		ch := map[string]interface{}{"path": path}
		switch act {
		case "delete":
			ch["delete"] = true
		case "update", "create":
			data, _ := a["content_base64"].(string)
			dec, derr := base64.StdEncoding.DecodeString(data)
			if derr != nil {
				dec = []byte(data)
			}
			ch["content"] = string(dec)
		}
		changes = append(changes, ch)
	}
	body := map[string]interface{}{
		"ref":     bookmark,
		"message": message,
		"changes": changes,
	}
	v, err := c.post(ctx, "/api/v1/repo/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/files", body)
	if err != nil {
		return nil, err
	}
	// easylab returns revision_id; alias it to change_id for the tool layer.
	if id, ok := v["revision_id"].(string); ok && v["change_id"] == nil {
		v["change_id"] = id
	}
	if sh, ok := v["snapshot"].(string); ok && v["sha"] == nil {
		v["sha"] = sh
	}
	return v, nil
}

func (c *easylabClient) put(ctx context.Context, path string, body interface{}) (map[string]interface{}, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.doJSON(req)
}

func (c *easylabClient) delete(ctx context.Context, path string, body interface{}) (map[string]interface{}, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.doJSON(req)
}

func (c *easylabClient) doJSON(req *http.Request) (map[string]interface{}, error) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, errNotFoundForHTTP)
	}
	if resp.StatusCode >= 400 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("%s %s: HTTP %d: %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	var raw json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s %s: decode response: %w", req.Method, req.URL.Path, err)
	}
	// easylab returns bare arrays for list endpoints; wrap them so the
	// map-based tool handlers read data uniformly under "_arr".
	return wrapJSON(raw), nil
}

// wrapJSON returns the decoded value as a map. A top-level JSON array is placed
// under the "_arr" key so callers that decode into map[string]any still get it.
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

// ---- merge requests (easylab native /merge_requests surface) ----

// GetBookmarkHead resolves a bookmark to its immutable revision id.
func (c *easylabClient) GetBookmarkHead(ctx context.Context, org, repo, bookmark string) (string, error) {
	status, v, err := c.call(ctx, http.MethodGet,
		"/api/v1/repo/"+url.PathEscape(org)+"/"+url.PathEscape(repo)+"/bookmarks/"+url.PathEscape(bookmark), nil)
	if err != nil {
		return "", errDownstream("easylab", err)
	}
	if status != 200 {
		return "", errDownstream("easylab", fmt.Errorf("get bookmark: HTTP %d %s", status, errText(v)))
	}
	if s, _ := v["revision_id"].(string); s != "" {
		return s, nil
	}
	return "", errDownstream("easylab", fmt.Errorf("get bookmark: no revision in response"))
}
