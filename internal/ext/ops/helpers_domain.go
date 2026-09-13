package opsext

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

func toJSON(v interface{}) string {
	b, err := jsonMarshalIndent(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func jsonMarshalIndent(v interface{}) ([]byte, error) {
	return marshalIndent(v)
}

func resourceRequestFromArgs(args map[string]interface{}) *ResourceRequest {
	rr := &ResourceRequest{}
	raw, ok := args["resources"].(map[string]interface{})
	if !ok {
		return rr
	}
	if reqs, ok := raw["requests"].(map[string]interface{}); ok {
		rr.Requests = &ResourcePair{
			CPU:    strArg(reqs, "cpu"),
			Memory: strArg(reqs, "memory"),
		}
	}
	if limits, ok := raw["limits"].(map[string]interface{}); ok {
		rr.Limits = &ResourcePair{
			CPU:    strArg(limits, "cpu"),
			Memory: strArg(limits, "memory"),
		}
	}
	return rr
}

func jobArgs(args map[string]interface{}) map[string]interface{} {
	out := map[string]interface{}{
		"job_id": strArg(args, "job-id"),
	}
	if v := args["start"]; v != nil {
		out["start"] = v
	}
	if v := args["end"]; v != nil {
		out["end"] = v
	}
	if g := strArg(args, "grep"); g != "" {
		out["grep"] = g
	}
	if v := args["timeout-ms"]; v != nil {
		out["timeout_ms"] = v
	}
	return out
}

func (s *server) fetchService(ctx context.Context, name, namespace string) (map[string]interface{}, error) {
	u := s.base + "/api/v1/ops/services/" + url.PathEscape(name)
	if namespace != "" {
		u += "?namespace=" + url.QueryEscape(namespace)
	}
	raw, err := s.httpGetJSON(ctx, u)
	if err != nil {
		// 404 (not found) is the normal "no existing service" case.
		if strings.Contains(err.Error(), "404") {
			return nil, nil
		}
		return nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, err
	}
	// Flatten annotations into the returned map under "session" for the
	// ownership check; callers only need easylab/session here.
	if ann, ok := out["annotations"].(map[string]interface{}); ok {
		if s, ok := ann["easylab/session"].(string); ok {
			out["session"] = s
		}
	}
	return out, nil
}

func (s *server) qualifyImage(ref, defaultTag string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ref
	}
	host := s.artifactImageHost
	// A registry host is present only when the first path segment (before the
	// first '/') looks like a host: contains '.' or a colon followed by a
	// numeric port, or equals "localhost". A bare "name:tag" (e.g. "example-
	// server:main") has no '.' and its colon is followed by a non-numeric tag,
	// so it is NOT a host and must be qualified.
	first := ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		first = ref[:i]
	}
	if isRegistryHost(first) {
		return ref
	}
	// Bare name, name:tag, or repo/name[:tag] — qualify with the artifact host.
	if !strings.Contains(ref, ":") {
		if defaultTag == "" {
			defaultTag = "latest"
		}
		ref += ":" + defaultTag
	}
	return host + "/" + ref
}

func rawMap(v interface{}) map[string]interface{} {
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func strArg(args map[string]interface{}, k string) string {
	if v, ok := args[k].(string); ok {
		return v
	}
	return ""
}

func intArg64(args map[string]interface{}, k string, def int64) int64 {
	if v, ok := args[k].(float64); ok {
		return int64(v)
	}
	return def
}

func boolArg(args map[string]interface{}, k string) bool {
	if v, ok := args[k].(bool); ok {
		return v
	}
	return false
}

func k8sServiceName(org, repo, bm string) string {
	s := strings.ToLower(org + "-" + repo + "-" + bm)
	// Replace characters that are invalid in a DNS-1123 label.
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '/':
			b.WriteByte('-')
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) <= 63 {
		return name
	}
	// Over-long: hash to stay within the label limit while remaining unique.
	h := sha256.Sum256([]byte(org + ":" + repo + ":" + bm))
	return fmt.Sprintf("svc-%x", h[:8])
}

func isRegistryHost(seg string) bool {
	if seg == "localhost" {
		return true
	}
	if strings.Contains(seg, ".") {
		return true
	}
	if i := strings.IndexByte(seg, ':'); i >= 0 {
		port := seg[i+1:]
		if port != "" {
			if _, err := strconv.Atoi(port); err == nil {
				return true
			}
		}
	}
	return false
}

func envMapFromArgs(args map[string]interface{}) map[string]string {
	if raw, ok := args["env"].(map[string]interface{}); ok {
		out := map[string]string{}
		for k, v := range raw {
			if s, ok := v.(string); ok {
				out[k] = s
			}
		}
		return out
	}
	return nil
}

func (s *server) sandboxEdit(ctx context.Context, tenant, sessionName, cid, path string, startLine, endLine int64, content string) (string, error) {
	data, err := s.sandboxFileRead(ctx, cid, path)
	if err != nil {
		return "", ef(ctx, s.ext, tenant, sessionName, "sandbox edit read failed: %v", "sandbox 编辑读取失败：%v", err)
	}
	current := string(data)
	lines := strings.Split(current, "\n")
	var newLines []string
	if content != "" {
		newLines = strings.Split(content, "\n")
	}
	if endLine < startLine {
		// insert before startLine (1-based)
		insertAt := int(startLine - 1)
		if insertAt > len(lines) {
			insertAt = len(lines)
		}
		v := append([]string{}, lines[:insertAt]...)
		v = append(v, newLines...)
		v = append(v, lines[insertAt:]...)
		lines = v
	} else {
		// replace [startLine, endLine] (1-based, inclusive)
		sIdx := int(startLine - 1)
		eIdx := int(endLine)
		if eIdx > len(lines) {
			eIdx = len(lines)
		}
		if sIdx > len(lines) {
			sIdx = len(lines)
		}
		v := append([]string{}, lines[:sIdx]...)
		v = append(v, newLines...)
		v = append(v, lines[eIdx:]...)
		lines = v
	}
	newContent := strings.Join(lines, "\n")
	if err := s.sandboxFileWrite(ctx, cid, path, []byte(newContent)); err != nil {
		return "", ef(ctx, s.ext, tenant, sessionName, "sandbox edit write failed: %v", "sandbox 编辑写入失败：%v", err)
	}
	return fmt.Sprintf("Edited sandbox file '%s'.", path), nil
}

func (s *server) portFile(ctx context.Context, tenant, sessionName string, sc sandboxCtx, args map[string]interface{}) (string, error) {
	sandboxPath := strArg(args, "sandbox-path")
	repoPath := strArg(args, "repo-path")
	message := strArg(args, "message")
	if message == "" {
		message = "port " + sandboxPath
	}

	commitsPath := fmt.Sprintf("%s/repo/%s/%s/commit",
		s.base, urlPathEscape(sc.ws.org), urlPathEscape(sc.ws.repo))

	// Determine whether sandbox_path is a directory.
	info, err := s.sandboxFileStat(ctx, sc.cid, sandboxPath)
	if err != nil {
		return "", ef(ctx, s.ext, tenant, sessionName, "port sandbox stat failed: %v", "沙箱 stat 失败：%v", err)
	}

	if !info.IsDir() {
		// Single file: read + optimistic-lock commit (one action).
		data, err := s.sandboxFileRead(ctx, sc.cid, sandboxPath)
		if err != nil {
			return "", ef(ctx, s.ext, tenant, sessionName, "port sandbox read failed: %v", "沙箱读取失败：%v", err)
		}
		commitBody := func(changes []map[string]interface{}) map[string]interface{} {
			return map[string]interface{}{
				"ref":         sc.ws.branchOrDefault(),
				"description": message,
				"new_commit":  true,
				"changes":     changes,
			}
		}
		var resp map[string]interface{}
		if err := s.httpPostJSONMap(ctx, commitsPath, commitBody([]map[string]interface{}{
			{"path": repoPath, "content": string(data)},
		}), &resp); err != nil {
			return "", ef(ctx, s.ext, tenant, sessionName, "port write failed: %v", "沙箱写入失败：%v", err)
		}
		changeID := strField(resp, "change_id")
		if changeID == "" {
			changeID = strField(resp, "commit_id")
		}
		return fmt.Sprintf("Ported '%s' to repo '%s' (change %s).", sandboxPath, repoPath, shortID(changeID)), nil
	}

	// Directory: expand via worker file_list, then one atomic commit (multi-action).
	files, err := s.sandboxFileList(ctx, sc.cid, sandboxPath)
	if err != nil {
		return "", ef(ctx, s.ext, tenant, sessionName, "port sandbox list failed: %v", "沙箱列表失败：%v", err)
	}
	if len(files) == 0 {
		return "", ef(ctx, s.ext, tenant, sessionName, "port sandbox directory '%s' is empty", "沙箱目录 '%s' 为空", sandboxPath)
	}
	changes := make([]map[string]interface{}, 0, len(files))
	for _, f := range files {
		rel := f["path"].(string)
		target := repoPath
		if target == "" || strings.HasSuffix(target, "/") {
			target = strings.TrimRight(repoPath, "/") + "/" + rel
		} else {
			target = target + "/" + rel
		}
		contentBase64, _ := f["content"].(string)
		changes = append(changes, map[string]interface{}{
			"path":    target,
			"content": base64Decode(contentBase64),
		})
	}
	var resp map[string]interface{}
	if err := s.httpPostJSONMap(ctx, commitsPath, map[string]interface{}{
		"ref":         sc.ws.branchOrDefault(),
		"description": message,
		"new_commit":  true,
		"changes":     changes,
	}, &resp); err != nil {
		return "", ef(ctx, s.ext, tenant, sessionName, "port commit write failed: %v", "提交写入失败：%v", err)
	}
	changeID := strField(resp, "change_id")
	if changeID == "" {
		changeID = strField(resp, "commit_id")
	}
	return fmt.Sprintf("Ported directory '%s' to repo '%s' (%d file(s), change %s).", sandboxPath, repoPath, len(changes), shortID(changeID)), nil
}

// branchOrDefault returns b or "main" when empty.
func branchOrDefault(b string) string {
	if b == "" {
		return "main"
	}
	return b
}
