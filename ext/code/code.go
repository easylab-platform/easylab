package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easylab-client-sdk"
	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
	"github.com/abcp-sdk/abc-protocol-go/extension"
)

// client bridges ext/code onto the easylab Lab API via the typed
// easylab-client-sdk (Connect). Bookmarks are gone upstream — they map to
// branches. A thin REST helper is retained only for tool-specific read-only
// endpoints that have no proto RPC yet (search / compare / history / blame),
// so the ext still works end-to-end.
type client struct {
	base string
	sdk  *easylabsdk.Client
}

func newClient(base string) *client {
	return &client{base: base, sdk: easylabsdk.New(base, envOr("EASYLAB_TOKEN", "devtoken"))}
}

func sessionOf(args map[string]any) (org, repo, rev string) {
	org = abcprotocol.ArgString(args, "org")
	repo = abcprotocol.ArgString(args, "repo")
	rev = abcprotocol.ArgString(args, "rev")
	return
}

func (c *client) list(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, rev := sessionOf(args)
	if org == "" || repo == "" {
		return toolResult("missing org/repo", map[string]any{}), nil
	}
	entries, err := c.sdk.ListReposTreeEntries(ctx, org, repo, rev, abcprotocol.ArgString(args, "path"))
	if err != nil {
		return extension.ToolResultData{}, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "entries (%d):\n", len(entries))
	for _, e := range entries {
		fmt.Fprintf(&sb, "  [%s] %s\n", e.Kind, e.Name)
	}
	return toolResult(sb.String(), map[string]any{"entries": entries}), nil
}

func (c *client) read(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, rev := sessionOf(args)
	path := abcprotocol.ArgString(args, "path")
	if path == "" {
		return toolResult("missing 'path'", map[string]any{}), nil
	}
	text, err := c.sdk.ReadBlobText(ctx, org, repo, path, rev)
	if err != nil {
		return extension.ToolResultData{}, err
	}
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
	return toolResult(text, map[string]any{"path": path, "size": len(text)}), nil
}

func (c *client) commit(ctx context.Context, org, repo, rev, message string, changes []map[string]any) (string, error) {
	// easylab-sdk WriteBlob writes one file per call; a multi-change commit is
	// modeled as successive writes onto the same branch.
	var rid string
	for _, ch := range changes {
		path, _ := ch["path"].(string)
		del, _ := ch["delete"].(bool)
		content, _ := ch["content"].(string)
		if del {
			content = ""
		} else {
			// content may be base64-encoded by the caller.
			if dec, e := base64.StdEncoding.DecodeString(content); e == nil {
				content = string(dec)
			}
		}
		ok, err := c.sdk.WriteBlob(ctx, org, repo, rev, path, content, message)
		if err != nil {
			return "", err
		}
		if ok {
			rid = path
		}
	}
	return rid, nil
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
	cur, err := c.sdk.ReadBlobText(ctx, org, repo, path, rev)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	lines := strings.Split(cur, "\n")
	newLines, err := applyLineEdit(lines, start, end, content)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	rid, err := c.commit(ctx, org, repo, rev, message, []map[string]any{
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
	q := abcprotocol.ArgString(args, "q")
	matches, err := c.sdk.Search(ctx, org, repo, rev, q)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d match(es):\n", len(matches))
	for _, mt := range matches {
		fmt.Fprintf(&sb, "  %s\n", mt)
	}
	return toolResult(sb.String(), map[string]any{"matches": matches}), nil
}

func (c *client) revisionDiff(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, _ := sessionOf(args)
	revA := abcprotocol.ArgString(args, "rev_a")
	revB := abcprotocol.ArgString(args, "rev_b")
	if revA != "" && revB != "" {
		diff, err := c.sdk.Compare(ctx, org, repo, revA, revB)
		if err != nil {
			return extension.ToolResultData{}, err
		}
		return toolResult(fmt.Sprintf("diff %s..%s:\n%s", revA, revB, diff), map[string]any{"rev_a": revA, "rev_b": revB}), nil
	}
	if rev := abcprotocol.ArgString(args, "rev"); rev != "" {
		diff, err := c.sdk.RevisionDiff(ctx, org, repo, rev)
		if err != nil {
			return extension.ToolResultData{}, err
		}
		return toolResult(fmt.Sprintf("diff of %s:\n%s", rev, diff), map[string]any{"rev": rev}), nil
	}
	return toolResult("need rev_a/rev_b or rev", map[string]any{}), nil
}

func (c *client) log(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, rev := sessionOf(args)
	revs, err := c.sdk.Revisions(ctx, org, repo, rev, 200)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d revision(s):\n", len(revs))
	for _, rv := range revs {
		desc := rv.GetMessage()
		if desc == "" {
			desc = rv.GetSha()
		}
		fmt.Fprintf(&sb, "  %s %s\n", shortID(rv.GetRev()), desc)
	}
	var meta []map[string]any
	for _, rv := range revs {
		meta = append(meta, map[string]any{
			"revision_id": rv.GetRev(), "description": rv.GetMessage(), "sha": rv.GetSha(),
		})
	}
	return toolResult(sb.String(), map[string]any{"revisions": meta}), nil
}

func (c *client) blame(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, rev := sessionOf(args)
	path := abcprotocol.ArgString(args, "path")
	lines, err := c.sdk.Blame(ctx, org, repo, path, rev)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "per-line origin of '%s':\n", path)
	for _, ln := range lines {
		content := ofStr(ln, "_content")
		fmt.Fprintf(&sb, "  %s | %s\n", shortID(ofStr(ln, "_rev")), content)
	}
	return toolResult(sb.String(), map[string]any{"lines": lines}), nil
}

func (c *client) refs(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, _ := sessionOf(args)
	bms, err := c.sdk.Branches(ctx, org, repo)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	tags, err := c.sdk.Tags(ctx, org, repo)
	if err != nil {
		return extension.ToolResultData{}, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "branches (%d):\n", len(bms))
	for _, b := range bms {
		fmt.Fprintf(&sb, "  %s -> %s\n", b.GetName(), shortID(b.GetSha()))
	}
	fmt.Fprintf(&sb, "tags (%d):\n", len(tags))
	for _, t := range tags {
		fmt.Fprintf(&sb, "  %s -> %s\n", t.GetName(), shortID(t.GetTarget()))
	}
	var bmsMeta []map[string]any
	for _, b := range bms {
		bmsMeta = append(bmsMeta, map[string]any{"name": b.GetName(), "revision_id": b.GetSha()})
	}
	var tagsMeta []map[string]any
	for _, t := range tags {
		tagsMeta = append(tagsMeta, map[string]any{"name": t.GetName(), "revision_id": t.GetTarget()})
	}
	return toolResult(sb.String(), map[string]any{"branches": bmsMeta, "tags": tagsMeta}), nil
}

func (c *client) history(ctx context.Context, args map[string]any, _ string, _ string) (extension.ToolResultData, error) {
	org, repo, _ := sessionOf(args)
	path := abcprotocol.ArgString(args, "path")
	edits, err := c.sdk.FileHistory(ctx, org, repo, path, "")
	if err != nil {
		return extension.ToolResultData{}, err
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d edit(s) of '%s':\n", len(edits), path)
	for _, e := range edits {
		fmt.Fprintf(&sb, "  %s %s\n", shortID(ofStr(e, "_rev")), ofStr(e, "_status"))
	}
	return toolResult(sb.String(), map[string]any{"edits": edits}), nil
}

func ofStr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

var _ = easylabv1.BranchInfo{}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
