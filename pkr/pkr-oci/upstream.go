package oci

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/pkr/pkrkit"
)

const acceptManifests = Accept

// Upstream is a remote OCI registry used as a pull-through source. It
// transparently acquires bearer tokens via the WWW-Authenticate challenge on
// 401/403, caches them per scope, and exposes manifest/blob/tag access.
type Upstream struct {
	scheme  string
	host    string
	factory *pkrkit.ClientFactory
	proxy   *string

	mu     sync.Mutex
	tokens map[string]string
}

// NewUpstream parses a host string (with or without scheme), defaulting to
// https unless the host is localhost or an IP literal (insecure convention).
func NewUpstream(factory *pkrkit.ClientFactory, host string, proxy *string) *Upstream {
	scheme := "https"
	h := host
	for _, p := range []string{"https://", "http://"} {
		if strings.HasPrefix(host, p) {
			scheme = strings.TrimSuffix(p, "://")
			h = strings.TrimPrefix(host, p)
			break
		}
	}
	return &Upstream{scheme: scheme, host: h, factory: factory, proxy: proxy, tokens: map[string]string{}}
}

// ForRegistry builds an upstream for an explicitly prefixed registry host,
// honoring the insecure-registry convention (http for localhost/IP).
func ForRegistry(factory *pkrkit.ClientFactory, host string, proxy *string) *Upstream {
	scheme := "https"
	h := host
	for _, p := range []string{"https://", "http://"} {
		if strings.HasPrefix(host, p) {
			scheme = strings.TrimSuffix(p, "://")
			h = strings.TrimPrefix(host, p)
			break
		}
	}
	ipHost := h
	if i := strings.Index(h, ":"); i >= 0 {
		ipHost = h[:i]
	}
	if h == "localhost" || strings.HasPrefix(h, "localhost:") || isIP(ipHost) {
		scheme = "http"
	}
	return &Upstream{scheme: scheme, host: h, factory: factory, proxy: proxy, tokens: map[string]string{}}
}

func isIP(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && c != '.' {
			return false
		}
	}
	return true
}

func (u *Upstream) schemeOf() string { return u.scheme }
func (u *Upstream) hostOf() string   { return u.host }

func (u *Upstream) base() string { return fmt.Sprintf("%s://%s/v2", u.scheme, u.host) }

func (u *Upstream) client() *http.Client { return u.factory.Client(u.proxy) }

func (u *Upstream) tokenFor(scope string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.tokens[scope]
}

func (u *Upstream) setToken(scope, tok string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.tokens[scope] = tok
}

// doGet GETs a path under /v2 with a scope, retrying once with a fresh token
// on 401/403.
func (u *Upstream) doGet(scope, path string, accept string) (*http.Response, error) {
	urlp := u.base() + path
	req, err := http.NewRequest(http.MethodGet, urlp, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", pkrkit.UserAgent)
	req.Header.Set("Accept", accept)
	if tok := u.tokenFor(scope); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		return resp, nil
	}
	// Acquire a fresh token for this scope and retry exactly once.
	challenge := resp.Header.Get("WWW-Authenticate")
	tok, err := u.fetchTokenForChallenge(challenge, scope)
	if err != nil {
		resp.Body.Close()
		return nil, err
	}
	u.setToken(scope, tok)
	resp.Body.Close()
	req2, err := http.NewRequest(http.MethodGet, urlp, nil)
	if err != nil {
		return nil, err
	}
	req2.Header.Set("User-Agent", pkrkit.UserAgent)
	req2.Header.Set("Accept", accept)
	req2.Header.Set("Authorization", "Bearer "+tok)
	return u.client().Do(req2)
}

// tokenResponse is the parsed token grant.
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
}

func (u *Upstream) fetchTokenForChallenge(challenge, scope string) (string, error) {
	params := parseAuthParams(challenge)
	if realm, ok := params["realm"]; ok {
		tok, err := u.fetchToken(realm, params, scope)
		if err == nil {
			return tok, nil
		}
	}
	// No usable challenge: probe /v2/ to discover it.
	resp, err := u.client().Get(u.base() + "/")
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	params = parseAuthParams(resp.Header.Get("WWW-Authenticate"))
	realm, ok := params["realm"]
	if !ok {
		return "", fmt.Errorf("unsupported challenge")
	}
	return u.fetchToken(realm, params, scope)
}

func (u *Upstream) fetchToken(realm string, params map[string]string, scope string) (string, error) {
	u2, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u2.Query()
	if scope != "" {
		q.Set("scope", scope)
	}
	if svc, ok := params["service"]; ok {
		q.Set("service", svc)
	}
	u2.RawQuery = q.Encode()
	req, err := http.NewRequest(http.MethodGet, u2.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", pkrkit.UserAgent)
	resp, err := u.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("token request failed: %d", resp.StatusCode)
	}
	var tr tokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", err
	}
	if tr.Token != "" {
		return tr.Token, nil
	}
	return tr.AccessToken, nil
}

func parseAuthParams(s string) map[string]string {
	// Strip an optional "Bearer " challenge-scheme prefix before splitting
	// on ','; otherwise the first key becomes "Bearer realm" and realm is
	// never found.
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "Bearer "))
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if i := strings.Index(part, "="); i >= 0 {
			k := strings.TrimSpace(part[:i])
			v := strings.Trim(strings.TrimSpace(part[i+1:]), "\"")
			out[k] = v
		}
	}
	return out
}

// GetManifest fetches a manifest by name+reference. Returns (body, content-type).
func (u *Upstream) GetManifest(name, reference string) ([]byte, string, error) {
	scope := "repository:" + name + ":pull"
	path := "/" + name + "/manifests/" + reference
	resp, err := u.doGet(scope, path, acceptManifests)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, "", &upstreamStatus{Status: resp.StatusCode}
	}
	body, err := readLimited(resp, 32<<20)
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/vnd.oci.image.manifest.v1+json"
	}
	return body, ct, nil
}

// GetBlob streams a blob by digest. Returns (response, content-length).
func (u *Upstream) GetBlob(name, digest string) (*http.Response, *int64, error) {
	scope := "repository:" + name + ":pull"
	path := "/" + name + "/blobs/" + digest
	resp, err := u.doGet(scope, path, "application/octet-stream")
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		resp.Body.Close()
		return nil, nil, &upstreamStatus{Status: resp.StatusCode}
	}
	var lenp *int64
	if cl := resp.ContentLength; cl >= 0 {
		lenp = &cl
	}
	return resp, lenp, nil
}

// ListTags returns the tag list for a repository.
func (u *Upstream) ListTags(name string) ([]string, error) {
	scope := "repository:" + name + ":pull"
	path := "/" + name + "/tags/list"
	resp, err := u.doGet(scope, path, acceptManifests)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &upstreamStatus{Status: resp.StatusCode}
	}
	body, err := readLimited(resp, 32<<20)
	if err != nil {
		return nil, err
	}
	var t struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, err
	}
	return t.Tags, nil
}

// readLimited reads the body with a byte cap.
func readLimited(resp *http.Response, capBytes int) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(capBytes)))
	if err != nil {
		return nil, err
	}
	if len(body) >= capBytes {
		return nil, fmt.Errorf("upstream body too large")
	}
	return body, nil
}

// upstreamStatus wraps a non-2xx upstream response.
type upstreamStatus struct{ Status int }

func (e *upstreamStatus) Error() string { return fmt.Sprintf("upstream status: %d", e.Status) }
