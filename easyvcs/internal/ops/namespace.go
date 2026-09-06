package ops

import (
	"os"
	"sort"
	"strings"
	"sync"
)

// NamespaceRegistry is the set of approved ops target namespaces plus the
// default. It mirrors jj-lab's namespace.rs: every /ops run/service/helm
// request must land in an approved namespace, and the HTTP layer gates it.
type NamespaceRegistry struct {
	mu        sync.RWMutex
	names     []string
	set       map[string]bool
	defaultNS string
}

// NewNamespaceRegistry builds a registry from an ordered list; the first entry
// is the default. An empty list yields a registry with only "default".
func NewNamespaceRegistry(names []string) *NamespaceRegistry {
	if len(names) == 0 {
		names = []string{"default"}
	}
	set := map[string]bool{}
	clean := make([]string, 0, len(names))
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !set[n] {
			set[n] = true
			clean = append(clean, n)
		}
	}
	return &NamespaceRegistry{names: clean, set: set, defaultNS: clean[0]}
}

// FromEnv builds a registry from EASYVCS_OPS_NAMESPACES (comma-separated).
func FromEnv() *NamespaceRegistry {
	raw := os.Getenv("EASYVCS_OPS_NAMESPACES")
	if strings.TrimSpace(raw) == "" {
		return NewNamespaceRegistry(nil)
	}
	return NewNamespaceRegistry(strings.Split(raw, ","))
}

// Default returns the default namespace.
func (r *NamespaceRegistry) Default() string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.defaultNS
}

// Approved reports whether a namespace is registered.
func (r *NamespaceRegistry) Approved(ns string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.set[ns]
}

// Resolve returns the effective namespace for a request: the explicit one if
// approved, otherwise the default. It reports false when no namespace is
// approved (which should not happen given the default is always present).
func (r *NamespaceRegistry) Resolve(ns string) (string, bool) {
	if ns == "" {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.defaultNS, true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.set[ns] {
		return ns, true
	}
	return "", false
}

// List returns the approved namespace names, sorted.
func (r *NamespaceRegistry) List() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := append([]string(nil), r.names...)
	sort.Strings(out)
	return out
}
