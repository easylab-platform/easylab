package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	abcprotocol "forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go"
	"forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/extension"
)

// client is a thin HTTP client for easyvcsd's Lab API.
type client struct {
	base string
	http *http.Client
}

func (c *client) do(ctx context.Context, method, path string, body any) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(c.base, "/")+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func sessionOf(args map[string]any) (org, repo, rev string) {
	org = abcprotocol.ArgString(args, "org")
	repo = abcprotocol.ArgString(args, "repo")
	rev = abcprotocol.ArgString(args, "rev")
	return
}

func repoOr(org, repo string) string {
	return url.PathEscape(org) + "/" + url.PathEscape(repo)
}

func escPath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

func (c *client) list(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, rev := sessionOf(args)
	if org == "" || repo == "" {
		return toolResult("missing org/repo", map[string]any{}), nil
	}
	q := url.Values{}
	if rev != "" {
		q.Set("ref", rev)
	}
	pathArg := abcprotocol.ArgString(args, "path")
	if pathArg != "" {
		q.Set("path", pathArg)
	}
	rd, err := c.do(ctx, "GET", "/api/v1/repositories/"+repoOr(org, repo)+"/tree?"+q.Encode(), nil)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	var out struct {
		Entries []struct {
			Name string `json:"name"`
			Kind string `json:"kind"`
			ID   string `json:"id"`
		} `json:"entries"`
	}
	_ = json.Unmarshal(rd, &out)
	var sb strings.Builder
	fmt.Fprintf(&sb, "entries (%d):\n", len(out.Entries))
	for _, e := range out.Entries {
		fmt.Fprintf(&sb, "  [%s] %s\n", e.Kind, e.Name)
	}
	return toolResult(sb.String(), map[string]any{"entries": out.Entries}), nil
}

func (c *client) read(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, rev := sessionOf(args)
	path := abcprotocol.ArgString(args, "path")
	if path == "" {
		return toolResult("missing 'path'", map[string]any{}), nil
	}
	q := url.Values{"path": []string{path}}
	if rev != "" {
		q.Set("ref", rev)
	}
	rd, err := c.do(ctx, "GET", "/api/v1/repositories/"+repoOr(org, repo)+"/blob?"+q.Encode(), nil)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	text := string(rd)
	offset := int(abcprotocol.ArgInt(args, "offset", 1))
	limit := int(abcprotocol.ArgInt(args, "limit", 0))
	if offset > 1 || limit > 0 {
		lines := strings.Split(text, "\n")
		start := offset - 1
		if start > len(lines) {
			start = len(lines)
		}
		end := len(lines)
		total := len(lines)
		trunc := false
		if limit > 0 && start+limit < end {
			end = start + limit
			trunc = true
		}
		text = strings.Join(lines[start:end], "\n")
		if trunc {
			text += fmt.Sprintf("\n... (%d lines total)", total)
		}
	}
	return toolResult(text, map[string]any{"path": path, "size": len(rd)}), nil
}

func (c *client) commit(ctx context.Context, org, repo, rev, message string, changes []map[string]any) (string, error) {
	body := map[string]any{
		"parent_hash": "", "description": message, "changes": changes,
	}
	rd, err := c.do(ctx, "POST", "/api/v1/repositories/"+repoOr(org, repo)+"/commits", body)
	if err != nil {
		return "", err
	}
	var out struct {
		RevisionID string `json:"revision_id"`
	}
	_ = json.Unmarshal(rd, &out)
	return out.RevisionID, nil
}

func (c *client) write(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, _ := sessionOf(args)
	path := abcprotocol.ArgString(args, "path")
	content := abcprotocol.ArgString(args, "content")
	message := abcprotocol.ArgString(args, "message")
	if message == "" {
		message = "write " + path
	}
	if path == "" {
		return toolResult("missing 'path'", map[string]any{}), nil
	}
	rid, err := c.commit(ctx, org, repo, "", message, []map[string]any{
		{"path": path, "content": content},
	})
	if err != nil {
		return extension.ToolResultData{}, err
	}
	return toolResult(fmt.Sprintf("wrote file '%s' (revision %s)", path, shortID(rid)), map[string]any{"revision_id": rid, "path": path}), nil
}

func (c *client) edit(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, rev := sessionOf(args)
	path := abcprotocol.ArgString(args, "path")
	start := int(abcprotocol.ArgInt(args, "start_line", 0))
	end := int(abcprotocol.ArgInt(args, "end_line", 0))
	content := abcprotocol.ArgString(args, "content")
	message := abcprotocol.ArgString(args, "message")
	if message == "" {
		message = "edit " + path
	}
	// Read current, apply line edit, commit.
	q := url.Values{"path": []string{path}}
	if rev != "" {
		q.Set("ref", rev)
	}
	rd, err := c.do(ctx, "GET", "/api/v1/repositories/"+repoOr(org, repo)+"/blob?"+q.Encode(), nil)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	lines := strings.Split(string(rd), "\n")
	// applyLineEdit: replace [start,end] with content (1-based inclusive).
	newLines, err := applyLineEdit(lines, start, end, content)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	rid, err := c.commit(ctx, org, repo, "", message, []map[string]any{
		{"path": path, "content": strings.Join(newLines, "\n")},
	})
	if err != nil {
		return extension.ToolResultData{}, err
	}
	return toolResult(fmt.Sprintf("edited '%s' lines %d-%d (revision %s)", path, start, end, shortID(rid)), map[string]any{"revision_id": rid, "path": path}), nil
}

func applyLineEdit(lines []string, start, end int, content string) ([]string, error) {
	if start < 1 {
		start = 1
	}
	if end < start {
		end = start
	}
	if start > len(lines) {
		start = len(lines)
	}
	if end > len(lines) {
		end = len(lines)
	}
	var out []string
	out = append(out, lines[:start-1]...)
	out = append(out, content)
	out = append(out, lines[end:]...)
	return out, nil
}

func (c *client) delete(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, _ := sessionOf(args)
	path := abcprotocol.ArgString(args, "path")
	message := abcprotocol.ArgString(args, "message")
	if message == "" {
		message = "delete " + path
	}
	rid, err := c.commit(ctx, org, repo, "", message, []map[string]any{
		{"path": path, "delete": true},
	})
	if err != nil {
		return extension.ToolResultData{}, err
	}
	return toolResult(fmt.Sprintf("deleted '%s' (revision %s)", path, shortID(rid)), map[string]any{"revision_id": rid, "path": path}), nil
}

func (c *client) search(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, rev := sessionOf(args)
	q0 := abcprotocol.ArgString(args, "q")
	q := url.Values{"q": []string{q0}}
	if rev != "" {
		q.Set("ref", rev)
	}
	rd, err := c.do(ctx, "GET", "/api/v1/repositories/"+repoOr(org, repo)+"/search?"+q.Encode(), nil)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	var out struct {
		Matches []map[string]any `json:"matches"`
	}
	_ = json.Unmarshal(rd, &out)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d match(es):\n", len(out.Matches))
	for _, mt := range out.Matches {
		fmt.Fprintf(&sb, "  %v\n", mt["path"])
	}
	return toolResult(sb.String(), map[string]any{"matches": out.Matches}), nil
}

func (c *client) revisionDiff(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, _ := sessionOf(args)
	revA := abcprotocol.ArgString(args, "rev_a")
	revB := abcprotocol.ArgString(args, "rev_b")
	if revA != "" && revB != "" {
		q := url.Values{"from": []string{revA}, "to": []string{revB}}
		rd, err := c.do(ctx, "GET", "/api/v1/repositories/"+repoOr(org, repo)+"/compare?"+q.Encode(), nil)
		if err != nil {
			return extension.ToolResultData{}, err
		}
		return toolResult(fmt.Sprintf("diff %s..%s:\n%s", revA, revB, string(rd)), map[string]any{"rev_a": revA, "rev_b": revB}), nil
	}
	if rev := abcprotocol.ArgString(args, "rev"); rev != "" {
		rd, err := c.do(ctx, "GET", "/api/v1/repositories/"+repoOr(org, repo)+"/revisions/"+url.PathEscape(rev)+"/diff", nil)
		if err != nil {
			return extension.ToolResultData{}, err
		}
		return toolResult(fmt.Sprintf("diff of %s:\n%s", rev, string(rd)), map[string]any{"rev": rev}), nil
	}
	return toolResult("need rev_a/rev_b or rev", map[string]any{}), nil
}

func (c *client) log(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, rev := sessionOf(args)
	q := url.Values{}
	if rev != "" {
		q.Set("ref", rev)
	}
	rd, err := c.do(ctx, "GET", "/api/v1/repositories/"+repoOr(org, repo)+"/revisions?"+q.Encode(), nil)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	var revs []map[string]any
	_ = json.Unmarshal(rd, &revs)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d revision(s):\n", len(revs))
	for _, rv := range revs {
		fmt.Fprintf(&sb, "  %s %s\n", shortID(strOf(rv, "revision_id")), strOf(rv, "description"))
	}
	return toolResult(sb.String(), map[string]any{"revisions": revs}), nil
}

func (c *client) blame(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, rev := sessionOf(args)
	path := abcprotocol.ArgString(args, "path")
	q := url.Values{"path": []string{path}}
	if rev != "" {
		q.Set("ref", rev)
	}
	rd, err := c.do(ctx, "GET", "/api/v1/repositories/"+repoOr(org, repo)+"/blame?"+q.Encode(), nil)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	var lines []map[string]any
	_ = json.Unmarshal(rd, &lines)
	var sb strings.Builder
	fmt.Fprintf(&sb, "per-line origin of '%s':\n", path)
	for _, ln := range lines {
		content, _ := ln["content"].(string)
		fmt.Fprintf(&sb, "  %s | %s\n", shortID(strOf(ln, "revision_id")), content)
	}
	return toolResult(sb.String(), map[string]any{"lines": lines}), nil
}

func (c *client) refs(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, _ := sessionOf(args)
	rd, err := c.do(ctx, "GET", "/api/v1/repositories/"+repoOr(org, repo)+"/bookmarks", nil)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	var bms []map[string]any
	_ = json.Unmarshal(rd, &bms)
	rd2, _ := c.do(ctx, "GET", "/api/v1/repositories/"+repoOr(org, repo)+"/tags", nil)
	var tags []map[string]any
	_ = json.Unmarshal(rd2, &tags)
	var sb strings.Builder
	fmt.Fprintf(&sb, "bookmarks (%d):\n", len(bms))
	for _, b := range bms {
		fmt.Fprintf(&sb, "  %s -> %s\n", strOf(b, "name"), shortID(strOf(b, "revision_id")))
	}
	fmt.Fprintf(&sb, "tags (%d):\n", len(tags))
	for _, t := range tags {
		fmt.Fprintf(&sb, "  %s -> %s\n", strOf(t, "name"), shortID(strOf(t, "revision_id")))
	}
	return toolResult(sb.String(), map[string]any{"bookmarks": bms, "tags": tags}), nil
}

func (c *client) history(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, _ := sessionOf(args)
	path := abcprotocol.ArgString(args, "path")
	q := url.Values{"path": []string{path}}
	rd, err := c.do(ctx, "GET", "/api/v1/repositories/"+repoOr(org, repo)+"/history?"+q.Encode(), nil)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	var edits []map[string]any
	_ = json.Unmarshal(rd, &edits)
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d edit(s) of '%s':\n", len(edits), path)
	for _, e := range edits {
		fmt.Fprintf(&sb, "  %s %s\n", shortID(strOf(e, "RevisionID")), strOf(e, "Status"))
	}
	return toolResult(sb.String(), map[string]any{"edits": edits}), nil
}

func strOf(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
