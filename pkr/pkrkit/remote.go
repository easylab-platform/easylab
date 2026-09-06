package pkrkit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// UserAgent is sent on every upstream request. Registries rate-limit generic
// user agents (Maven Central 429s the default Go UA), so a stable bespoke one
// is used.
const UserAgent = "pkrkit/1.0 (pull-through mirror)"

// ClientFactory builds (and caches) an http.Client for a proxy policy:
//   - nil           -> follow the environment proxy configuration
//   - Some("")      -> direct connection (env proxy bypassed)
//   - Some(url)     -> always route through the given proxy
type ClientFactory struct {
	clients map[string]*http.Client
}

// NewClientFactory returns an empty factory.
func NewClientFactory() *ClientFactory {
	return &ClientFactory{clients: map[string]*http.Client{}}
}

// Client returns a cached client for the proxy policy.
func (f *ClientFactory) Client(proxy *string) *http.Client {
	key := "__env__"
	if proxy != nil {
		key = *proxy
	}
	if c, ok := f.clients[key]; ok {
		return c
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 20
	tr.IdleConnTimeout = 90 * time.Second
	tr.ResponseHeaderTimeout = 120 * time.Second
	if proxy != nil {
		if *proxy == "" {
			tr.Proxy = nil // direct
		} else {
			tr.Proxy = http.ProxyURL(mustParseURL(*proxy))
		}
	}
	c := &http.Client{
		Transport: tr,
		// No overall Timeout: large blobs stream for minutes.
	}
	f.clients[key] = c
	return c
}

// Remote is a proxy-aware upstream root URL. Adapters map their URL layout
// onto Get/GetBytes/GetCached.
type Remote struct {
	Base    string
	client  *http.Client
	headers map[string]string
}

// NewRemote creates a Remote for a base URL (trailing slash trimmed).
func NewRemote(factory *ClientFactory, base string, proxy *string) *Remote {
	return &Remote{Base: trimSlash(base), client: factory.Client(proxy)}
}

// WithHeader returns a copy with an extra header (e.g. Accept, auth).
func (r *Remote) WithHeader(k, v string) *Remote {
	cp := *r
	cp.headers = map[string]string{}
	for a, b := range r.headers {
		cp.headers[a] = b
	}
	cp.headers[k] = v
	return &cp
}

// URL joins a path onto the base.
func (r *Remote) URL(path string) string { return r.Base + path }

// Get issues GET and returns the raw response (non-2xx returned as-is).
func (r *Remote) Get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.URL(path), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", UserAgent)
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	return r.client.Do(req)
}

// GetBytes GETs and errors on non-2xx, returning the body bytes.
func (r *Remote) GetBytes(ctx context.Context, path string) ([]byte, error) {
	resp, err := r.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &UpstreamStatusError{Path: path, Status: resp.StatusCode}
	}
	return io.ReadAll(resp.Body)
}

// GetCached GETs through a TTL cache keyed by absolute URL (for small index
// documents re-fetched on every client resolution).
func (r *Remote) GetCached(ctx context.Context, cache *MemCache, path string) (string, error) {
	key := r.URL(path)
	if body, ok := cache.Get(key); ok {
		return body, nil
	}
	body, err := r.GetBytes(ctx, path)
	if err != nil {
		return "", err
	}
	s := string(body)
	cache.Set(key, s)
	return s, nil
}

// UpstreamStatusError is the shared non-2xx upstream error.
type UpstreamStatusError struct {
	Path   string
	Status int
}

func (e *UpstreamStatusError) Error() string {
	return fmt.Sprintf("upstream %s: status %d", e.Path, e.Status)
}

// Registry implementation ----------------------------------------------------

// Fetch pulls `path` from an upstream and caches it in the blob store.
func (r *Registry) Fetch(ctx context.Context, format, upstreamBase, path string) (Fetched, error) {
	remote, err := r.remote(ctx, format, upstreamBase)
	if err != nil {
		return Fetched{}, err
	}
	resp, err := remote.Get(ctx, path)
	if err != nil {
		return Fetched{}, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Fetched{}, &UpstreamStatusError{Path: path, Status: resp.StatusCode}
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Fetched{}, err
	}
	return r.finishFetch(ctx, data)
}

// FetchAbsolute pulls a full URL verbatim.
func (r *Registry) FetchAbsolute(ctx context.Context, url string) (Fetched, error) {
	factory := NewClientFactory()
	remote := NewRemote(factory, "", proxyPtr(r.Upstreams, "generic"))
	resp, err := remote.Get(ctx, url)
	if err != nil {
		return Fetched{}, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Fetched{}, &UpstreamStatusError{Path: url, Status: resp.StatusCode}
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return Fetched{}, err
	}
	return r.finishFetch(ctx, data)
}

// StoreAndHash writes into the blob store (dedup by sha256) and returns a summary.
func (r *Registry) StoreAndHash(ctx context.Context, data []byte) (Stored, error) {
	h, _ := ComputeHashesBytes(data)
	digest := "sha256:" + h.SHA256
	if _, err := r.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(data)); err != nil {
		return Stored{}, err
	}
	return Stored{Hashes: h, Size: int64(len(data)), Digest: digest}, nil
}

// Remote returns a proxy-aware upstream handle for a format. When
// upstreamBase is "" the format's configured/default upstream is used.
func (r *Registry) Remote(format, upstreamBase string) (*Remote, error) {
	base := upstreamBase
	if base == "" {
		base = r.Upstreams.Get(format)
		if base == "" {
			return nil, fmt.Errorf("no upstream for %s", format)
		}
	}
	factory := NewClientFactory()
	return NewRemote(factory, base, proxyPtr(r.Upstreams, format)), nil
}

// RemoteAt returns a remote against an arbitrary absolute base using the
// format's proxy policy.
func (r *Registry) RemoteAt(base string) *Remote {
	factory := NewClientFactory()
	return NewRemote(factory, base, proxyPtr(r.Upstreams, "generic"))
}

// proxyPtr returns a *string to the configured proxy for key, or nil when
// unset (meaning "follow environment proxy"). An explicitly configured empty
// string means "direct" and produces a pointer to "".
func proxyPtr(u *Upstreams, key string) *string {
	v, ok := u.ProxyURL(key)
	if !ok {
		return nil
	}
	return &v
}

func (r *Registry) remote(ctx context.Context, format, upstreamBase string) (*Remote, error) {
	return r.Remote(format, upstreamBase)
}

func (r *Registry) finishFetch(ctx context.Context, data []byte) (Fetched, error) {
	stored, err := r.StoreAndHash(ctx, data)
	if err != nil {
		return Fetched{}, err
	}
	return Fetched{Data: data, Hashes: stored.Hashes, Size: stored.Size}, nil
}
