package pkrkit

import "net/http"

// Action is the coarse permission an adapter requests from upstream.
type Action string

const (
	ActionPull   Action = "pull"
	ActionPush   Action = "push"
	ActionDelete Action = "delete"
)

// Protocol is the pluggable seam for third-party protocol adapters. An
// adapter implements Register so the registry knows its format name; the
// concrete handler is a http.Handler mounted at a caller-chosen prefix.
//
// Implementations must only depend on *pkrkit.Registry and never on other
// protocol packages. This invariant is what keeps the multi-repository
// module graph free of version conflicts.
type Protocol interface {
	// Name returns the protocol format key ("oci", "pypi", "cargo", ...).
	Name() string
	// New builds the adapter for a substrate and configuration, or errors.
	New(reg *Registry, cfg map[string]any) (http.Handler, error)
}

// ProtocolEntry couples a protocol's name to a constructor for the registry.
type ProtocolEntry struct {
	Name string
	New  func(reg *Registry, cfg map[string]any) (http.Handler, error)
}

// registry is the process-global compile-time protocol registry. Third-party
// adapters call Register in their init() or a callable; cmd-pkr imports the
// adapter package (blank import) to pull it in.
var registry = map[string]func(reg *Registry, cfg map[string]any) (http.Handler, error){}

// Register adds a protocol constructor. It panics on protocol-name collisions
// at start-up time, which is a programmer error we want surfaced eagerly.
func Register(name string, build func(reg *Registry, cfg map[string]any) (http.Handler, error)) {
	if _, ok := registry[name]; ok {
		panic("pkrkit: protocol already registered: " + name)
	}
	registry[name] = build
}

// Build constructs a protocol handler from its registered constructor.
func Build(name string, reg *Registry, cfg map[string]any) (http.Handler, error) {
	build, ok := registry[name]
	if !ok {
		return nil, ErrProtocolUnknown(name)
	}
	return build(reg, cfg)
}

// Registered returns the names of all registered protocols.
func Registered() []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	return out
}
