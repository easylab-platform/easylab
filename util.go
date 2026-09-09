package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

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
