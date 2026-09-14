package opsext

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/easylab-platform/easylab/internal/ext"
)

// artifactClient is the h1-native HTTP client for the artifact registry
// (/v2, /pkgs): podman/skopeo/npm ecosystems are h1-native.
var artifactClient = &http.Client{Timeout: 60 * time.Second}

// httpGetJSONArtifact fetches from the artifact registry (/v2, /pkgs) over
// HTTP/1.1 and returns the pretty-printed JSON body.
func (s *server) httpGetJSONArtifact(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	s.addAuth(req)
	resp, err := artifactClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var v interface{}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("GET %s: %d %s", redactURL(url), resp.StatusCode, toJSON(v))
	}
	return toJSON(v), nil
}

// addAuth attaches the appropriate credential to a request: easylab routes get
// the write token as a Bearer credential (identical to every other caller —
// the gateway accepts Bearer/token/bare uniformly); artifact-registry routes
// get the artifact token.
//
// The tenant of the caller rides X-Agent-Tenant from the request context
// (attached per tool call / lifecycle event), so easylab scopes the operation
// to the right tenant. It is honored only from loopback.
func (s *server) addAuth(req *http.Request) {
	if t := ext.LabTenantOf(req.Context()); t != "" {
		req.Header.Set("X-Agent-Tenant", t)
	}
	u := req.URL.String()
	isEasylab := strings.HasPrefix(u, s.base+"/") || u == s.base
	if isEasylab && s.easylabToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.easylabToken)
		return
	}
	if strings.HasPrefix(u, s.artifact+"/") && s.artifactToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.artifactToken)
	}
}

// redactURL strips query strings from URLs before embedding them in errors.
func redactURL(u string) string {
	if i := strings.IndexByte(u, '?'); i >= 0 {
		return u[:i]
	}
	return u
}

// selfBase returns the HTTP base of this instance for self-invoking build.
func selfBase() string {
	return "http://127.0.0.1:" + envOr("PORT", "8080")
}
