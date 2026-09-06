package pkrkit

import "context"

// clientFactoryFor caches a single ClientFactory on the schema-less Upstreams
// (per-process). Upstreams is cloned around, so we store the factory in an
// atomic side table rather than a struct field.
var globalFactory = NewClientFactory()

func clientFactoryFor(u *Upstreams) *ClientFactory { return globalFactory }

// Registry is the composed substrate handed to every protocol adapter. It
// bundles the metadata store (mutable index), the blob store (immutable CAS)
// and the upstream table, plus the generic pull-through flows. Adapters only
// depend on the interfaces, so a caller can wire any implementation.
type Registry struct {
	Blobs     BlobStore
	Meta      IndexStore
	Upstreams *Upstreams
}

// Upstream describes a remote base URL for a protocol (or a sub-endpoint).
type Upstream struct {
	Base    string // "" means the format's configured/default upstream
	Proxy   string // "" = direct, otherwise a proxy URL
	Default *string
}

// Upstreams resolves the effective remote for a format / sub-endpoint against
// per-key overrides and per-key proxy policy, honoring an air-gap flag.
type Upstreams struct {
	// Defaults maps a format to its built-in public upstream base.
	Defaults map[string]string
	// Overrides overrides Defaults; empty value reverts to the default.
	Overrides map[string]string
	// Proxy is per-key proxy URL. "" means direct; absence means env proxy.
	Proxy map[string]string
	// AirGap, when true, returns no upstreams at all (local-only registry).
	AirGap bool
}

// Get returns the effective base URL for a format, or "" when disabled.
func (u *Upstreams) Get(format string) string {
	if u.AirGap {
		return ""
	}
	if v, ok := u.Overrides[format]; ok {
		if v != "" {
			return trimSlash(v)
		}
	}
	return trimSlash(u.Defaults[format])
}

// Sub returns the effective base URL for a dotted sub-endpoint.
func (u *Upstreams) Sub(format, sub string) string {
	key := format + "." + sub
	if u.AirGap {
		return ""
	}
	if v, ok := u.Overrides[key]; ok {
		if v != "" {
			return trimSlash(v)
		}
	}
	return trimSlash(u.Defaults[key])
}

// ProxyURL returns the proxy policy for a key, walking dotted parents.
func (u *Upstreams) ProxyURL(key string) (string, bool) {
	for {
		if v, ok := u.Proxy[key]; ok {
			return v, true
		}
		for i := len(key) - 1; i >= 0; i-- {
			if key[i] == '.' {
				key = key[:i]
				goto next
			}
		}
		return "", false
	next:
	}
}

// ProxyFactory returns a client factory honored by the Upstreams' proxy
// policy. Adapters use it to build Remote handles.
func (u *Upstreams) ProxyFactory() *ClientFactory { return clientFactoryFor(u) }

// All returns the effective upstream for every known format + sub-endpoint.
func (u *Upstreams) All() []UpstreamEntry {
	var out []UpstreamEntry
	seen := map[string]bool{}
	for f := range u.Defaults {
		if v := u.Get(f); v != "" && !seen[f] {
			out = append(out, UpstreamEntry{Name: f, URL: v})
			seen[f] = true
		}
	}
	// Sub-endpoints whose key starts with "format.".
	for k := range u.Defaults {
		seen[k] = true
	}
	for k := range u.Overrides {
		if i := indexDot(k); i > 0 {
			sub := u.Sub(k[:i], k[i+1:])
			if sub != "" {
				out = append(out, UpstreamEntry{Name: k, URL: sub})
			}
		}
	}
	return out
}

// IsOverride reports whether a key has an explicit (non-default) override.
func (u *Upstreams) IsOverride(key string) bool { return u.Overrides[key] != "" }

// Set overrides a format/sub-endpoint URL and persists it in the in-memory map.
func (u *Upstreams) Set(key, url string) {
	if key == "" {
		return
	}
	u.Overrides[key] = url
}

// Reset reverts a key override to its default.
func (u *Upstreams) Reset(key string) {
	delete(u.Overrides, key)
}

// SetProxy stores a per-key proxy URL ("" = direct).
func (u *Upstreams) SetProxy(key, value string) {
	if key == "" {
		return
	}
	u.Proxy[key] = value
}

// ProxyStates returns the proxy policy for every known key.
func (u *Upstreams) ProxyStates() []ProxyState {
	var out []ProxyState
	for _, e := range u.All() {
		p, ok := u.ProxyURL(e.Name)
		out = append(out, ProxyState{Key: e.Name, Proxy: p, Explicit: ok})
	}
	return out
}

// UpstreamEntry is one effective mapping in All().
type UpstreamEntry struct {
	Name string `json:"key"`
	URL  string `json:"url"`
}

// ProxyState is the per-key proxy policy in ProxyStates().
type ProxyState struct {
	Key      string `json:"key"`
	Proxy    string `json:"proxy"`
	Explicit bool   `json:"explicit"`
}

func indexDot(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// Fetched is the result of a pull-through fetch: full upstream bytes (cached
// or fresh) plus hashes.
type Fetched struct {
	Data   []byte
	Hashes Hashes
	Size   int64
}

// Stored is the summary of bytes written into the CAS.
type Stored struct {
	Hashes Hashes
	Size   int64
	Digest string
}

// RegistryApi is a convenience trait object so adapters can hold either the
// concrete Registry or a narrowed view, and accept a blob sink other than the
// default.
type RegistryApi interface {
	// Fetch pulls `path` from an upstream, stores it (dedup by sha256) and
	// returns bytes + hashes. An empty upstreamBase means "use the format's
	// default"; non-empty bases are used verbatim.
	Fetch(ctx context.Context, format, upstreamBase, path string) (Fetched, error)
	// FetchAbsolute pulls a full URL verbatim.
	FetchAbsolute(ctx context.Context, url string) (Fetched, error)
	// StoreAndHash writes bytes into the blob CAS and returns their summary.
	StoreAndHash(ctx context.Context, data []byte) (Stored, error)
}
