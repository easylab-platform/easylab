package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
)

// strVal extracts a string field from a worker RPC reply.
func strVal(v map[string]interface{}) string {
	if s, ok := v["content"].(string); ok {
		return s
	}
	if s, ok := v["output"].(string); ok {
		return s
	}
	if s, ok := v["result"].(string); ok {
		return s
	}
	return ""
}

// strField extracts a named string field from a JSON map reply.
func strField(v map[string]interface{}, k string) string {
	if s, ok := v[k].(string); ok {
		return s
	}
	return ""
}

// shortID abbreviates a long id (commit/change sha) for readable output.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// errNotFoundForHTTP marks a 404-style not-found (mirrors repo-extension).
var errNotFoundForHTTP = errors.New("not found")

// param extracts a URL param from a chi-routed request.
func param(r *http.Request, key string) string {
	return chi.URLParam(r, key)
}

func parseIntOr(s string, def int64) int64 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func marshalIndent(v interface{}) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

func urlPathEscape(s string) string {
	return url.PathEscape(s)
}

// escapePath escapes each path segment, keeping slashes as separators.
func escapePath(p string) string {
	p = strings.TrimPrefix(p, "/")
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// hostOf extracts host[:port] from a base URL, dropping default ports so the
// result is usable inside image references (FROM/push tags).
func hostOf(rawURL string) string {
	h := rawURL
	if u, err := url.Parse(rawURL); err == nil && u.Host != "" {
		h = u.Host
	} else {
		h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
		if i := strings.IndexByte(h, '/'); i >= 0 {
			h = h[:i]
		}
	}
	h = strings.TrimSuffix(h, ":80")
	h = strings.TrimSuffix(h, ":443")
	return h
}

func trimTrailingSlash(s string) string {
	return strings.TrimRight(s, "/")
}

// sandboxFileRead reads a file from the worker via the native file_read RPC
// (base64 round-trip, binary-safe). The minimal worker shell has no pipes or
// redirection, so `cat` is only suitable for display, not data transport.
func (s *server) sandboxFileRead(ctx context.Context, cid, path string) ([]byte, error) {
	res, err := s.workerCommand(ctx, cid, "file_read", map[string]interface{}{"path": path})
	if err != nil {
		return nil, fmt.Errorf("file_read %s: %w", path, err)
	}
	b64 := strVal(rawMap(res))
	if b64 == "" && rawMap(res)["content"] == nil {
		return nil, fmt.Errorf("file_read %s: no content in reply", path)
	}
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("file_read %s: bad base64: %w", path, err)
	}
	return data, nil
}

// sandboxFileWrite writes a file via the native file_write RPC (creates
// parent directories itself).
func (s *server) sandboxFileWrite(ctx context.Context, cid, path string, data []byte) error {
	_, err := s.workerCommand(ctx, cid, "file_write", map[string]interface{}{
		"path":    path,
		"content": base64.StdEncoding.EncodeToString(data),
	})
	if err != nil {
		return fmt.Errorf("file_write %s: %w", path, err)
	}
	return nil
}

// sandboxFileStat stats a sandbox path via the worker file_list RPC (which
// returns `is_dir` for the path). For a single trailing-slash-less file the
// list has one entry but is_dir is false, so the flag (not the count) drives
// the directory branch.
func (s *server) sandboxFileStat(ctx context.Context, cid, path string) (os.FileInfo, error) {
	res, err := s.workerCommand(ctx, cid, "file_list", map[string]interface{}{"path": path})
	if err != nil {
		return nil, fmt.Errorf("file_list %s: %w", path, err)
	}
	isDir, _ := rawMap(res)["is_dir"].(bool)
	return syntheticFileInfo{isDir: isDir}, nil
}

// sandboxFileList returns the files under a sandbox path (a directory, or the
// single file) as a list of {path, size, content(base64)}.
func (s *server) sandboxFileList(ctx context.Context, cid, path string) ([]map[string]interface{}, error) {
	res, err := s.workerCommand(ctx, cid, "file_list", map[string]interface{}{"path": path})
	if err != nil {
		return nil, fmt.Errorf("file_list %s: %w", path, err)
	}
	files, _ := rawMap(res)["files"].([]interface{})
	out := make([]map[string]interface{}, 0, len(files))
	for _, f := range files {
		if m, ok := f.(map[string]interface{}); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// syntheticFileInfo is a minimal os.FileInfo for sandboxFileStat.
type syntheticFileInfo struct{ isDir bool }

func (s syntheticFileInfo) Name() string       { return "" }
func (s syntheticFileInfo) Size() int64        { return 0 }
func (s syntheticFileInfo) Mode() os.FileMode  { return 0 }
func (s syntheticFileInfo) ModTime() time.Time { return time.Time{} }
func (s syntheticFileInfo) IsDir() bool        { return s.isDir }
func (s syntheticFileInfo) Sys() interface{}   { return nil }

// inferRepoFromGitURL mirrors the old registry helper: strip scheme, trailing
// slash and ".git", then take the last path segment as the repo name.
func inferRepoFromGitURL(u string) string {
	s := strings.TrimRight(strings.TrimSpace(u), "/")
	s = strings.TrimSuffix(s, ".git")
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return s
}
