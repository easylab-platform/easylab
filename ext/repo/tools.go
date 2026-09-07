package main

import (
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	easylabsdk "github.com/easylab-platform/easylab-client-sdk"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"

	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
	"github.com/abcp-sdk/abc-protocol-go/extension"
)

// handlers binds each repo tool to its implementation, forwarding to the
// easylab REST surface. Descriptions/schemas live in manifest.yaml (the single
// declarative protocol source); each handler is bound by tool name.
func (s *server) handlers() map[string]extension.ToolSpec {
	// readFileRaw fetches a file and returns (raw utf8, sha, size) or error.
	// easylab responds with `encoding: base64` + base64 `content` (Gitea shape),
	// never with a plain-text body.
	readFileRaw := func(ctx context.Context, o, r, b, path string) (string, string, int64, error) {
		data, err := s.sdk.ReadBlob(ctx, o, r, path, b)
		if err != nil {
			return "", "", 0, err
		}
		text := string(data)
		return text, "", int64(len(text)), nil
	}

	// fileBody builds the atomic-commit action body for a create/update.
	// (unused inline helper retained for clarity in write/edit paths below.)

	return map[string]extension.ToolSpec{
		"read": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.refBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				path := abcprotocol.ArgString(args, "path")
				if path == "" {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "missing 'path' argument", "缺少 'path' 参数")
				}
				text, sha, size, err := readFileRaw(ctx, o, r, b, path)
				if err != nil {
					if errors.Is(err, errNotFoundForHTTP) {
						return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("failed to read file '%s': not found or inaccessible", path), fmt.Sprintf("读取文件 '%s' 失败：未找到或不可访问", path))}, nil
					}
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "read '%s': %v", "读取 '%s'：%v", path, err)
				}
				offset := abcprotocol.ArgInt(args, "offset", 1)
				limit := abcprotocol.ArgInt(args, "limit", 0)
				if offset < 1 {
					offset = 1
				}

				lines := strings.Split(text, "\n")
				if len(lines) > 0 && lines[len(lines)-1] == "" {
					lines = lines[:len(lines)-1]
				}
				totalLines := int64(len(lines))

				startIdx := offset - 1
				if startIdx > int64(len(lines)) {
					return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("file '%s' has %d lines; offset=%d is past the end.", path, totalLines, offset), fmt.Sprintf("文件 '%s' 共 %d 行；offset=%d 已超出末尾。", path, totalLines, offset)), Data: map[string]interface{}{
						"path": path, "sha": sha, "size": size, "total_lines": totalLines, "truncated": false,
					}}, nil
				}

				endIdx := int64(len(lines))
				truncated := false
				if limit > 0 {
					want := startIdx + limit
					if want < endIdx {
						endIdx = want
						truncated = true
					}
				}

				var sb strings.Builder
				for i := startIdx; i < endIdx; i++ {
					fmt.Fprintf(&sb, "%d: %s\n", i+1, lines[i])
				}
				content := sb.String()
				nextOffset := endIdx + 1
				if truncated {
					fmt.Fprintf(&sb, "\n(file not fully read: showing lines %d-%d of %d; continue with offset=%d)\n", startIdx+1, endIdx, totalLines, nextOffset)
					content = sb.String()
				}

				meta := map[string]interface{}{
					"path": path, "sha": sha, "size": size, "total_lines": totalLines, "truncated": truncated,
				}
				if truncated {
					meta["next_offset"] = nextOffset
				}
				return extension.ToolResultData{Content: content, Data: meta}, nil
			},
		},
		"write": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				path := abcprotocol.ArgString(args, "path")
				if path == "" {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "missing 'path' argument", "缺少 'path' 参数")
				}
				content := abcprotocol.ArgString(args, "content")
				message := abcprotocol.ArgString(args, "message")
				if message == "" {
					message = "write " + path
				}
				// Optimistic-lock: read the file's current blob sha (if it
				// exists) and pass it as the base so a concurrent change is
				// rejected rather than silently overwritten. A new file passes
				// no sha (easylab treats missing base as "no lock").
				baseSha := ""
				if oldText, oldSha, _, rerr := readFileRaw(ctx, o, r, b, path); rerr == nil {
					_ = oldText
					baseSha = oldSha
				}
				v, err := s.lab.commit(ctx, o, r, b, message, []map[string]interface{}{
					{"action": "update", "path": path, "content_base64": base64.StdEncoding.EncodeToString([]byte(content)), "sha": baseSha},
				})
				if err != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "failed to write file: %v", "写入文件失败：%v", err)
				}
				changeID := strVal(v, "change_id")
				return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("wrote file '%s' (change %s)", path, shortID(changeID)), fmt.Sprintf("已写入文件 '%s'（变更 %s）", path, shortID(changeID))), Data: map[string]interface{}{
					"path": path, "change_id": changeID, "base_sha": baseSha,
				}}, nil
			},
		},
		"delete": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				path := abcprotocol.ArgString(args, "path")
				if path == "" {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "missing 'path' argument", "缺少 'path' 参数")
				}
				message := abcprotocol.ArgString(args, "message")
				if message == "" {
					message = "delete " + path
				}
				// Optimistic-lock base: read the file's current blob sha so a
				// concurrent change/replacement is rejected (409) not clobbered.
				baseSha := ""
				if _, oldSha, _, rerr := readFileRaw(ctx, o, r, b, path); rerr == nil {
					baseSha = oldSha
				}
				body := map[string]interface{}{"action": "delete", "path": path, "sha": baseSha}
				v, err := s.lab.commit(ctx, o, r, b, message, []map[string]interface{}{body})
				if err != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "failed to delete file: %v", "删除文件失败：%v", err)
				}
				changeID := strVal(v, "change_id")
				return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("deleted file '%s' (change %s)", path, shortID(changeID)), fmt.Sprintf("已删除文件 '%s'（变更 %s）", path, shortID(changeID))), Data: map[string]interface{}{
					"path": path, "change_id": changeID, "base_sha": baseSha,
				}}, nil
			},
		},
		"edit": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				path := abcprotocol.ArgString(args, "path")
				if path == "" {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "missing 'path' argument", "缺少 'path' 参数")
				}
				startLine := abcprotocol.ArgInt(args, "start-line", 0)
				endLine := abcprotocol.ArgInt(args, "end-line", 0)
				content := abcprotocol.ArgString(args, "content")
				message := abcprotocol.ArgString(args, "message")
				if message == "" {
					message = "edit " + path
				}

				text, sha, _, err := readFileRaw(ctx, o, r, b, path)
				if err != nil {
					if errors.Is(err, errNotFoundForHTTP) {
						return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "failed to read file '%s': not found or inaccessible", "读取文件 '%s' 失败：未找到或不可访问", path)
					}
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "read '%s' before edit: %v", "编辑前读取 '%s'：%v", path, err)
				}

				newContent, err := applyLineEdit(text, startLine, endLine, content)
				if err != nil {
					return extension.ToolResultData{}, err
				}

				// Optimistic-lock: pass the blob sha we read as the base so a
				// concurrent edit is rejected (409) instead of clobbered.
				v, err := s.lab.commit(ctx, o, r, b, message, []map[string]interface{}{
					{"action": "update", "path": path, "content_base64": base64.StdEncoding.EncodeToString([]byte(newContent)), "sha": sha},
				})
				if err != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "failed to write edited result: %v", "写入编辑结果失败：%v", err)
				}
				changeID := strVal(v, "change_id")

				var desc string
				if endLine < startLine {
					desc = fmt.Sprintf("inserted %d line(s) before line %d", startLine, countLines(content))
				} else {
					desc = fmt.Sprintf("replaced lines %d-%d", startLine, endLine)
				}
				return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("edited file '%s': %s (change %s)", path, desc, shortID(changeID)), fmt.Sprintf("已编辑文件 '%s'：%s（变更 %s）", path, desc, shortID(changeID))), Data: map[string]interface{}{
					"path": path, "start-line": startLine, "end-line": endLine,
					"old_sha": sha, "change_id": changeID, "diff": diffLines(text, newContent),
				}}, nil
			},
		},
		"ls": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.refBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				path := abcprotocol.ArgString(args, "path")
				files, err := s.sdk.ListReposTreeEntries(ctx, o, r, b, path)
				if err != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "failed to list directory: %v", "列出目录失败：%v", err)
				}
				entries := treeEntries(files)
				dirs, nfiles := 0, 0
				for _, e := range entries {
					if e.isDir {
						dirs++
					} else {
						nfiles++
					}
				}
				max := abcprotocol.ArgInt(args, "max", 0)
				truncated := false
				if max > 0 && int64(len(entries)) > max {
					entries = entries[:max]
					truncated = true
				}
				var sb strings.Builder
				if path == "" {
					fmt.Fprintf(&sb, "rev '%s' has %d entries (%d dirs, %d files):\n", b, len(entries), dirs, nfiles)
				} else {
					fmt.Fprintf(&sb, "rev '%s' path '%s' has %d entries (%d dirs, %d files):\n", b, path, len(entries), dirs, nfiles)
				}
				for _, e := range entries {
					if e.isDir {
						fmt.Fprintf(&sb, "  [dir] %s/\n", e.path)
					} else {
						fmt.Fprintf(&sb, "  %s\n", e.path)
					}
				}
				if truncated {
					fmt.Fprintf(&sb, "\n(entries truncated; specify a smaller path or higher max)\n")
				}
				return extension.ToolResultData{Content: sb.String(), Data: map[string]interface{}{"entries": entriesSlice(entries), "truncated": truncated}}, nil
			},
		},
		"grep": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.refBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				pattern := abcprotocol.ArgString(args, "pattern")
				if pattern == "" {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "missing 'pattern' argument", "缺少 'pattern' 参数")
				}
				matches, err := s.sdk.Search(ctx, o, r, b, pattern)
				if err != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "search failed: %v", "搜索失败：%v", err)
				}
				if len(matches) == 0 {
					return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("no matches for '%s' in rev '%s'.", b, pattern), fmt.Sprintf("在版本 '%s' 中未找到 '%s' 的匹配。", b, pattern)), Data: map[string]interface{}{"matches": []interface{}{}, "count": 0}}, nil
				}
				max := abcprotocol.ArgInt(args, "max", 0)
				truncated := false
				if max > 0 && int64(len(matches)) > max {
					matches = matches[:max]
					truncated = true
				}
				var sb strings.Builder
				fmt.Fprintf(&sb, "found %d match(es):\n", len(matches))
				for _, m := range matches {
					fmt.Fprintf(&sb, "  %s\n", m)
				}
				if truncated {
					fmt.Fprintf(&sb, "\n(results truncated; narrow the pattern or path)\n")
				}
				parsed := make([]interface{}, 0, len(matches))
				for _, m := range matches {
					path, line, text := splitMatch(m)
					parsed = append(parsed, map[string]interface{}{"path": path, "line": line, "text": text})
				}
				return extension.ToolResultData{Content: sb.String(), Data: map[string]interface{}{"matches": parsed, "count": len(matches), "truncated": truncated}}, nil
			},
		},
		"explore": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				tree, err := s.lab.GetRepoTree(ctx)
				if err != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "failed to browse structure: %v", "浏览结构失败：%v", err)
				}
				orgArg := abcprotocol.ArgString(args, "org")
				repoArg := abcprotocol.ArgString(args, "repo")
				keyword := abcprotocol.ArgString(args, "keyword")

				var sb strings.Builder
				meta := []interface{}{}
				for org, repos := range tree {
					if orgArg != "" && org != orgArg {
						continue
					}
					if keyword != "" && !strings.Contains(org, keyword) {
						continue
					}
					fmt.Fprintf(&sb, "organization '%s' (%d repo(s)):\n", org, len(repos))
					rmeta := []interface{}{}
					for repo, bms := range repos {
						if repoArg != "" && repo != repoArg {
							continue
						}
						if keyword != "" && !strings.Contains(repo, keyword) {
							continue
						}
						fmt.Fprintf(&sb, "  - %s (bookmarks: %s)\n", repo, bookmarkTargets(bms))
						rmeta = append(rmeta, map[string]interface{}{"repo": repo, "bookmarks": bookmarkMeta(bms)})
					}
					meta = append(meta, map[string]interface{}{"org": org, "repos": rmeta})
				}
				if len(meta) == 0 {
					return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, "no organizations or repositories.", "没有组织或仓库。"), Data: map[string]interface{}{"orgs": []interface{}{}}}, nil
				}
				return extension.ToolResultData{Content: sb.String(), Data: map[string]interface{}{"orgs": meta}}, nil
			},
		},
		"vcs-graph": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, _, err := s.refBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				limit := abcprotocol.ArgInt(args, "limit", 0)
				nodes, err := s.sdk.Graph(ctx, o, r, int32(limit))
				if err != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "failed to get graph: %v", "获取图失败：%v", err)
				}
				arr := graphNodeMaps(nodes)
				if len(arr) == 0 {
					return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, "no commits in graph.", "图中无提交。"), Data: map[string]interface{}{"graph": []interface{}{}}}, nil
				}
				var sb strings.Builder
				fmt.Fprintf(&sb, "commit graph (%d nodes):\n", len(arr))
				meta := make([]interface{}, 0, len(arr))
				for _, m := range arr {
					isHead, _ := m["is_head"].(bool)
					commit := strFrom(m, "commit_id")
					bmks := bookmarkLabels(m["bookmarks"])
					fmt.Fprintf(&sb, "  %s %s%s%s\n", shortID(commit), strFrom(m, "message"), bmks, headLabel(isHead))
					meta = append(meta, map[string]interface{}{
						"commit_id": commit, "change_id": strFrom(m, "change_id"),
						"message": strFrom(m, "message"), "author": strFrom(m, "author"),
						"parents": m["parents"], "is_head": isHead, "bookmarks": m["bookmarks"],
					})
				}
				return extension.ToolResultData{Content: sb.String(), Data: map[string]interface{}{"graph": meta}}, nil
			},
		},
		"vcs-diff": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, _, err := s.sessionBaseXO(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				revA := abcprotocol.ArgString(args, "rev-a")
				revB := abcprotocol.ArgString(args, "rev-b")
				if revA == "" || revB == "" {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "rev_a and rev_b are required", "rev_a 与 rev_b 均为必填")
				}
				path := abcprotocol.ArgString(args, "path")
				files, err := s.sdk.Compare(ctx, o, r, revA, revB)
				if err != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "failed to get diff: %v", "获取差异失败：%v", err)
				}
				diff := compareDiffText(files)
				scope := "tree"
				if path != "" {
					diff = diffForPath(diff, path)
					scope = fmt.Sprintf("file '%s'", path)
				}
				if strings.TrimSpace(diff) == "" {
					return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("no diff between '%s' and '%s' (%s).", revA, revB, scope), fmt.Sprintf("'%s' 与 '%s' 之间无差异（%s）。", revA, revB, scope)), Data: map[string]interface{}{"path": path, "rev-a": revA, "rev-b": revB}}, nil
				}
				return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("diff (%s) between '%s'..'%s':\n%s", scope, revA, revB, diff), fmt.Sprintf("差异（%s）介于 '%s'..'%s'：\n%s", scope, revA, revB, diff)), Data: map[string]interface{}{"path": path, "rev-a": revA, "rev-b": revB}}, nil
			},
		},
		"vcs-rebase": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				source := abcprotocol.ArgString(args, "source")
				if source == "" {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "missing 'source' argument", "缺少 'source' 参数")
				}
				destSha, derr := s.lab.GetBookmarkHead(ctx, o, r, b)
				if derr != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "rebase failed: %v", "变基失败：%v", derr)
				}
				changeID, commitID, err := s.lab.Rebase(ctx, o, r, source, []string{destSha})
				if err != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "rebase failed: %v", "变基失败：%v", err)
				}
				conflicts := []string{}
				if len(conflicts) > 0 {
					return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("rebased '%s' onto '%s' with %d conflict(s): %s", source, b, len(conflicts), strings.Join(conflicts, ", ")), fmt.Sprintf("已将 '%s' 变基到 '%s' 上，共 %d 个冲突：%s", source, b, len(conflicts), strings.Join(conflicts, ", "))), Data: map[string]interface{}{"commit_id": commitID, "change_id": changeID, "conflicts": conflicts}}, nil
				}
				return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("rebased '%s' onto '%s' (tip %s, change %s).", source, b, shortID(commitID), shortID(changeID)), fmt.Sprintf("已将 '%s' 变基到 '%s'（尖端 %s，变更 %s）。", source, b, shortID(commitID), shortID(changeID))), Data: map[string]interface{}{"commit_id": commitID, "change_id": changeID, "conflicts": []interface{}{}}}, nil
			},
		},
		"vcs-resolve": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.sessionBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				path := abcprotocol.ArgString(args, "path")
				if path == "" {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "missing 'path' argument", "缺少 'path' 参数")
				}
				content := abcprotocol.ArgString(args, "content")
				message := "resolve " + path
				// git-resolve rewrites a conflicted file; include its current
				// blob sha as the optimistic-lock base so overlays on a stale
				// conflict are rejected too.
				baseSha := ""
				if _, oldSha, _, rerr := readFileRaw(ctx, o, r, b, path); rerr == nil {
					baseSha = oldSha
				}
				v, err := s.lab.commit(ctx, o, r, b, message, []map[string]interface{}{
					{"action": "update", "path": path, "content_base64": base64.StdEncoding.EncodeToString([]byte(content)), "sha": baseSha},
				})
				if err != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "resolve failed: %v", "解析失败：%v", err)
				}
				commitID := strVal(v, "sha")
				changeID := strVal(v, "change_id")
				return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("resolved '%s' (tip %s, change %s).", path, shortID(commitID), shortID(changeID)), fmt.Sprintf("已解析 '%s'（尖端 %s，变更 %s）。", path, shortID(commitID), shortID(changeID))), Data: map[string]interface{}{"commit_id": commitID, "change_id": changeID, "conflicts": []interface{}{}}}, nil
			},
		},
		"vcs-blame": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, _, err := s.refBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				rev := abcprotocol.ArgString(args, "rev")
				path := abcprotocol.ArgString(args, "path")
				if rev == "" {
					rev = "main"
				}
				if path == "" {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "missing 'path' argument", "缺少 'path' 参数")
				}
				v, err := s.lab.Blame(ctx, o, r, path, rev)
				if err != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "failed to get blame: %v", "获取 blame 失败：%v", err)
				}
				anns := mapBlameLines(v)
				var sb strings.Builder
				fmt.Fprintf(&sb, "per-line origin of file '%s':\n", path)
				meta := make([]interface{}, 0, len(anns))
				for _, a := range anns {
					m, ok := a.(map[string]interface{})
					if !ok {
						continue
					}
					commit := strFrom(m, "commit_id")
					change := strFrom(m, "change_id")
					content, _ := m["content"].(string)
					// Show the change-id (the semantic unit) when available.
					owner := change
					if owner == "" {
						owner = commit
					}
					fmt.Fprintf(&sb, "  %s: %s", shortID(owner), content)
					if !strings.HasSuffix(content, "\n") {
						fmt.Fprintln(&sb)
					}
					meta = append(meta, map[string]interface{}{"commit_id": commit, "change_id": change, "content": content})
				}
				return extension.ToolResultData{Content: sb.String(), Data: map[string]interface{}{"lines": meta}}, nil
			},
		},
		"vcs-log": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, b, err := s.refBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				limit := abcprotocol.ArgInt(args, "limit", 50)
				revs, err := s.sdk.Revisions(ctx, o, r, b, int32(limit))
				if err != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "failed to get commit history: %v", "获取提交历史失败：%v", err)
				}
				commits := revisionCommits(revs)
				var sb strings.Builder
				if len(commits) == 0 {
					return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, "no commits.", "无提交。"), Data: map[string]interface{}{"commits": []interface{}{}}}, nil
				}
				fmt.Fprintf(&sb, "latest %d commit(s):\n", len(commits))
				meta := make([]interface{}, 0, len(commits))
				for _, c := range commits {
					msg := c.message
					if msg == "" {
						msg = "(no description)"
					}
					fmt.Fprintf(&sb, "  %s %s（%s）\n", shortID(c.changeID), msg, c.author)
					meta = append(meta, map[string]interface{}{"change_id": c.changeID, "message": c.message, "author": c.author})
				}
				return extension.ToolResultData{Content: sb.String(), Data: map[string]interface{}{"commits": meta}}, nil
			},
		},
		"vcs-show": {
			Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
				o, r, _, err := s.refBase(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				rev := abcprotocol.ArgString(args, "rev")
				if rev == "" {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "missing 'rev' argument", "缺少 'rev' 参数")
				}
				files, err := s.lab.Diff(ctx, o, r, rev, "")
				if err != nil {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "failed to view change: %v", "查看变更失败：%v", err)
				}
				patch := diffFilesText(files)
				if strings.TrimSpace(patch) == "" {
					return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("change '%s' has no content diff.", rev), fmt.Sprintf("变更 '%s' 没有内容差异。", rev)), Data: map[string]interface{}{"rev": rev}}, nil
				}
				return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("changes of '%s':\n%s", rev, patch), fmt.Sprintf("'%s' 的变更：\n%s", rev, patch)), Data: map[string]interface{}{"rev": rev, "patch": patch}}, nil
			},
		},
	}
}

// sessionBase resolves the (org, repo, bookmark) triple for a tool call from
// the first-class `session_name` envelope field. There is no legacy
// `_org`/`_repo`/`_bookmark` fallback: the agent always carries the session
// name, and the workspace mapping is derived from it.
func (s *server) sessionBase(ctx context.Context, args map[string]interface{}, sessionName string) (string, string, string, error) {
	if sessionName == "" {
		return "", "", "", fmt.Errorf("missing session context (session_name)")
	}
	return s.resolveSession(ctx, sessionName)
}

// refBase resolves a `ref` argument into (org, repo, rev). `ref` is a full
// `org:repo:<rev>` path (never a bare bookmark/rev name) — the rev segment is
// passed verbatim to easylab, whose resolve_snapshot interprets it as a
// bookmark, commit hash, tag or change-id. Absent/empty `ref` defaults to the
// current workspace's (org, repo, bookmark); a `ref` that does not include an
// org:repo prefix is rejected (a bare `<rev>` cannot locate a repo).
func (s *server) refBase(ctx context.Context, args map[string]interface{}, sessionName string) (string, string, string, error) {
	ref := abcprotocol.ArgString(args, "ref")
	if ref == "" {
		// Default: current workspace (org:repo:bookmark) from session_name.
		if sessionName == "" {
			return "", "", "", fmt.Errorf("missing session context (session_name)")
		}
		return s.resolveSession(ctx, sessionName)
	}
	// Split org:repo:rev — rev may itself contain ':' (change-id, or a path
	// like feat/x), so split on the FIRST two colons only.
	parts := strings.SplitN(ref, ":", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("ref must be a full 'org:repo:<rev>' path (got %q)", ref)
	}
	org, repo, rev := parts[0], parts[1], parts[2]
	if org == "" || repo == "" || rev == "" {
		return "", "", "", fmt.Errorf("ref must be a full 'org:repo:<rev>' path (got %q)", ref)
	}
	return org, repo, rev, nil
}

// sessionBaseXO is sessionBase with an explicit `org`/`repo`/`bookmark` override
// (read-only cross-repo access); the bookmark still defaults to the workspace.
func (s *server) sessionBaseXO(ctx context.Context, args map[string]interface{}, sessionName string) (string, string, string, error) {
	o, r, b, err := s.sessionBase(ctx, args, sessionName)
	if err != nil {
		return "", "", "", err
	}
	if arg := abcprotocol.ArgString(args, "org"); arg != "" {
		o = arg
	}
	if arg := abcprotocol.ArgString(args, "repo"); arg != "" {
		r = arg
	}
	if arg := abcprotocol.ArgString(args, "bookmark"); arg != "" {
		b = arg
	}
	return o, r, b, nil
}

func escPath(p string) string {
	p = strings.TrimPrefix(p, "/")
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// mapBlameLines converts easylab's /blame []AnnotationLine into the
// [{commit_id, change_id, content}] shape the vcs-blame tool uses.
func mapBlameLines(v map[string]interface{}) []interface{} {
	arr, _ := v["_arr"].([]interface{})
	if len(arr) == 0 {
		arr, _ = v["annotations"].([]interface{})
	}
	var out []interface{}
	for _, a := range arr {
		m, ok := a.(map[string]interface{})
		if !ok {
			continue
		}
		revID := strFrom(m, "RevisionID")
		if revID == "" {
			revID = strFrom(m, "revision_id")
		}
		content := strFrom(m, "Content")
		if content == "" {
			content = strFrom(m, "content")
		}
		out = append(out, map[string]interface{}{"commit_id": revID, "change_id": revID, "content": content})
	}
	return out
}

// renderFileDiffs renders easylab's /compare or /revisions/{rev}/diff JSON
// array (FileDiff entries) into a plain unified patch string.
func renderFileDiffs(v map[string]interface{}) string {
	if v == nil {
		return ""
	}
	arr, _ := v["_arr"].([]interface{})
	if len(arr) == 0 {
		arr, _ = v["diffs"].([]interface{})
	}
	var b strings.Builder
	for _, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		content := strFrom(m, "Content")
		if content == "" {
			content = strFrom(m, "content")
		}
		path := strFrom(m, "Path")
		if path == "" {
			path = strFrom(m, "path")
		}
		if content != "" {
			b.WriteString("diff --git a/" + path + " b/" + path + "\n")
			b.WriteString(content)
			b.WriteString("\n")
		}
	}
	return b.String()
}

func strVal(v map[string]interface{}, k string) string {
	if s, ok := v[k].(string); ok {
		return s
	}
	return ""
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// bookmarkLabels renders the bookmarks attached to a graph commit as
// " (branch: a, b)" — empty when the commit carries no bookmark.
func bookmarkLabels(v interface{}) string {
	arr, ok := v.([]interface{})
	if !ok {
		return ""
	}
	names := make([]string, 0, len(arr))
	for _, e := range arr {
		if s, ok := e.(string); ok && s != "" {
			names = append(names, s)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return " (" + strings.Join(names, ", ") + ")"
}

// headLabel renders " [HEAD]" when the commit is a visible head.
func headLabel(isHead bool) string {
	if isHead {
		return " [HEAD]"
	}
	return ""
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	return len(strings.Split(s, "\n"))
}

func strSlice(v map[string]interface{}, k string) []string {
	if arr, ok := v[k].([]interface{}); ok {
		out := make([]string, 0, len(arr))
		for _, e := range arr {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func arraySlice(v interface{}) []interface{} {
	if arr, ok := v.([]interface{}); ok {
		return arr
	}
	return nil
}

// splitMatch splits "path:line:text" into (path, line, text); line/text are
// empty when the backend omits them.
func splitMatch(m string) (string, string, string) {
	i := strings.IndexByte(m, ':')
	if i == -1 {
		return m, "", ""
	}
	path := m[:i]
	rest := m[i+1:]
	j := strings.IndexByte(rest, ':')
	if j == -1 {
		return path, "", rest
	}
	return path, rest[:j], rest[j+1:]
}

// diffForPath filters a unified diff down to the entries for a single path.
// The backend's `compare` returns a whole-tree patch; Gitea has no path filter
// on compare, so the tool narrows client-side.
func diffForPath(diff, path string) string {
	header := "a/" + path + " b/" + path
	var out []string
	for _, block := range strings.Split(diff, "\ndiff --git ") {
		trimmed := block
		if i := strings.Index(trimmed, "\n"); i >= 0 {
			if trimmed[:i] == header {
				out = append(out, "diff --git "+trimmed)
			}
		}
	}
	return strings.Join(out, "\n")
}

// diffLines produces a small unified diff between two file contents (by line),
// or "" when identical. It is a best-effort tool-visible patch so the agent
// sees exactly what changed on an edit/write. Symmetric add/remove of whole
// lines; hunks are minimal (no context) to keep output compact.
func diffLines(oldText, newText string) string {
	if oldText == newText {
		return ""
	}
	oldLines := strings.Split(strings.TrimSuffix(oldText, "\n"), "\n")
	newLines := strings.Split(strings.TrimSuffix(newText, "\n"), "\n")
	if len(oldLines) == 1 && oldLines[0] == "" {
		oldLines = nil
	}
	if len(newLines) == 1 && newLines[0] == "" {
		newLines = nil
	}
	var sb strings.Builder
	sb.WriteString("--- a/file\n+++ b/file\n")
	// Simple line diff via LCS-free common prefix/suffix trim for readability.
	i := 0
	for i < len(oldLines) && i < len(newLines) && oldLines[i] == newLines[i] {
		i++
	}
	j := 0
	for j < len(oldLines)-i && j < len(newLines)-i && oldLines[len(oldLines)-1-j] == newLines[len(newLines)-1-j] {
		j++
	}
	del := oldLines[i : len(oldLines)-j]
	add := newLines[i : len(newLines)-j]
	start := i
	oldEnd := i + len(del)
	newEnd := i + len(add)
	fmt.Fprintf(&sb, "@@ -%d,%d +%d,%d @@\n", start, len(del), start, len(add))
	for _, l := range del {
		sb.WriteString("-" + l + "\n")
	}
	for _, l := range add {
		sb.WriteString("+" + l + "\n")
	}
	_ = oldEnd
	_ = newEnd
	return sb.String()
}

type entry struct {
	path  string
	isDir bool
	size  int64
}

// pathWithSlash normalizes a path argument into a leading-slash segment for
// the /contents/{path} subpath ("" -> "").
func pathWithSlash(path string) string {
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		return "/" + path
	}
	return path
}

func toEntries(v map[string]interface{}) []entry {
	arr, ok := v["tree"].([]interface{})
	if !ok {
		arr, ok = v["entries"].([]interface{})
	}
	if !ok {
		return nil
	}
	out := make([]entry, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		p, _ := m["path"].(string)
		isDir := false
		// easylab contents entries use `type` ("file"/"dir"); some surfaces also
		// carry `kind` ("tree"/"dir"). `kind`/`type` may be absent or nil
		// (interface zero value), so read them nil-safely.
		kind, _ := m["kind"].(string)
		switch kind {
		case "tree", "dir":
			isDir = true
		}
		if t, _ := m["type"].(string); t == "tree" || t == "dir" {
			isDir = true
		}
		if p == "" {
			if n, _ := m["name"].(string); n != "" {
				p = n
			}
		}
		var size int64
		if s, ok := m["size"].(float64); ok {
			size = int64(s)
		}
		out = append(out, entry{path: p, isDir: isDir, size: size})
	}
	return out
}

func entriesSlice(entries []entry) []interface{} {
	out := make([]interface{}, 0, len(entries))
	for _, e := range entries {
		typ := "blob"
		if e.isDir {
			typ = "tree"
		}
		out = append(out, map[string]interface{}{"path": e.path, "type": typ, "size": e.size})
	}
	return out
}

type commitInfo struct {
	changeID string
	message  string
	author   string
}

func toCommits(v map[string]interface{}) []commitInfo {
	arr, ok := v["commits"].([]interface{})
	if !ok {
		arr, _ = v["_arr"].([]interface{})
	}
	if len(arr) == 0 {
		return nil
	}
	out := make([]commitInfo, 0, len(arr))
	for _, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		out = append(out, commitInfo{
			changeID: strFrom(m, "revision_id"),
			message:  strFrom(m, "description"),
			author:   strFrom(m, "author"),
		})
	}
	return out
}

// bookmarkTargets renders a repo's bookmarks as "name(target)" joined for text.
func bookmarkTargets(bms []bookmarkInfo) string {
	parts := make([]string, 0, len(bms))
	for _, b := range bms {
		parts = append(parts, fmt.Sprintf("%s(%s)", b.Name, shortID(b.Sha)))
	}
	return strings.Join(parts, ", ")
}

// bookmarkMeta converts a repo's bookmarks into a Data slice with name+target.
func bookmarkMeta(bms []bookmarkInfo) []interface{} {
	out := make([]interface{}, 0, len(bms))
	for _, b := range bms {
		out = append(out, map[string]interface{}{"bookmark": b.Name, "sha": b.Sha})
	}
	return out
}

func strFrom(m map[string]interface{}, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

// applyLineEdit applies a line-based insert/replace to `current`, mirroring the
// original editor tools. start_line is 1-based; end_line < start_line means
// insert before start_line.
func applyLineEdit(current string, startLine, endLine int64, content string) (string, error) {
	endsNewline := strings.HasSuffix(current, "\n")
	body := strings.TrimSuffix(current, "\n")
	var lines []string
	if body == "" {
		lines = nil
	} else {
		lines = strings.Split(body, "\n")
	}
	total := len(lines)
	if startLine <= 0 {
		return "", fmt.Errorf("start_line must be >= 1")
	}
	if startLine > int64(total)+1 {
		return "", fmt.Errorf("start_line=%d out of range (file has %d lines)", startLine, total)
	}
	if endLine < startLine {
		insertAt := int(startLine - 1)
		if insertAt > total {
			insertAt = total
		}
		var result []string
		result = append(result, lines[:insertAt]...)
		if content != "" {
			result = append(result, strings.Split(content, "\n")...)
		}
		result = append(result, lines[insertAt:]...)
		joined := strings.Join(result, "\n")
		if endsNewline {
			joined += "\n"
		}
		return joined, nil
	}
	if endLine > int64(total)+1 {
		return "", fmt.Errorf("end_line=%d out of range (file has %d lines)", endLine, total)
	}
	s := int(startLine - 1)
	e := int(endLine)
	var result []string
	result = append(result, lines[:s]...)
	if content != "" {
		result = append(result, strings.Split(content, "\n")...)
	}
	result = append(result, lines[e:]...)
	joined := strings.Join(result, "\n")
	if endsNewline {
		joined += "\n"
	}
	return joined, nil
}

// toInt64 safely coerces a JSON-decoded number-ish value.
func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

// ---- proto → tool-shape converters ----

// treeEntries converts SDK FileEntry list into the ls tool's entry shape.
func treeEntries(files []easylabsdk.FileEntry) []entry {
	out := make([]entry, 0, len(files))
	for _, f := range files {
		out = append(out, entry{path: f.Path, isDir: f.Kind == "dir" || f.Kind == "tree"})
	}
	return out
}

// revisionCommits converts proto revisions into the history tool's commit shape.
func revisionCommits(revs []*easylabv1.RevisionInfo) []commitInfo {
	out := make([]commitInfo, 0, len(revs))
	for _, rv := range revs {
		out = append(out, commitInfo{
			changeID: rv.GetRev(),
			author:   rv.GetAuthor(),
			message:  rv.GetMessage(),
		})
	}
	return out
}

// diffFilesText renders proto DiffFile list as the unified patch text.
func diffFilesText(files []*easylabv1.DiffFile) string {
	var sb strings.Builder
	for _, f := range files {
		sb.WriteString(f.GetDiff())
	}
	return sb.String()
}

// compareDiffText renders Compare DiffFile list (tree scope).
func compareDiffText(files []*easylabv1.DiffFile) string {
	var sb strings.Builder
	for _, f := range files {
		sb.WriteString(f.GetDiff())
	}
	return sb.String()
}

// graphNodeMaps converts proto graph nodes into the loose map shape the graph
// tool renders from.
func graphNodeMaps(nodes []*easylabv1.GraphNode) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(nodes))
	for _, n := range nodes {
		parents := make([]interface{}, 0, len(n.GetParents()))
		for _, p := range n.GetParents() {
			parents = append(parents, p)
		}
		out = append(out, map[string]interface{}{
			"revision_id": n.GetRevisionId(),
			"change_id":   n.GetRevisionId(),
			"commit_id":   n.GetSnapshot(),
			"message":     n.GetMessage(),
			"author":      n.GetAuthor(),
			"parents":     parents,
			"is_head":     n.GetIsHead(),
		})
	}
	return out
}
