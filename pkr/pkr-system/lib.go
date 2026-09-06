// Package system implements the cross-protocol admin API (/pkgs/system):
// upstream overrides, per-key proxy policy, and package enumeration/deletion.
// Mirror of the pkglab-core system router so an embedder can wire any
// substrate. Lives in its own module because it is inherently cross-protocol.
package system

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/pkr/pkrkit"
)

// State carries the registry + auth for the admin endpoints.
type State struct {
	Registry *pkrkit.Registry
	Auth     pkrkit.Auth
}

// NewHandler is the pkrkit.Register constructor.
func NewHandler(reg *pkrkit.Registry, cfg map[string]any) (http.Handler, error) {
	s := &State{Registry: reg}
	if a, ok := cfg["auth"].(pkrkit.Auth); ok {
		s.Auth = a
	}
	return s, nil
}

func init() { pkrkit.Register("system", NewHandler) }

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := stringsTrimPrefix(r.URL.Path, "/pkgs/system")
	path = stringsTrimPrefix(path, "/system")

	switch {
	case path == "/upstreams" || path == "/upstreams/":
		s.listUpstreams(w, r)
	case len(path) > len("/upstreams/") && strings.HasPrefix(path, "/upstreams/"):
		key := stringsTrimPrefix(path, "/upstreams/")
		s.upstreamKey(w, r, key)
	case path == "/proxy" || path == "/proxy/":
		s.listProxy(w, r)
	case len(path) > len("/proxy/") && strings.HasPrefix(path, "/proxy/"):
		key := stringsTrimPrefix(path, "/proxy/")
		s.proxyKey(w, r, key)
	case path == "/packages" || path == "/packages/":
		s.packages(w, r)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *State) lookup(key string) (string, bool) {
	// Dotted sub-endpoint first, then bare format.
	if i := indexByte(key, '.'); i > 0 {
		sub := s.Registry.Upstreams.Sub(key[:i], key[i+1:])
		if sub != "" {
			return sub, true
		}
	}
	v := s.Registry.Upstreams.Get(key)
	return v, v != ""
}

func (s *State) listUpstreams(w http.ResponseWriter, r *http.Request) {
	all := map[string]string{}
	for _, k := range s.Registry.Upstreams.All() {
		all[k.Name] = k.URL
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"upstreams": all})
}

func (s *State) upstreamKey(w http.ResponseWriter, r *http.Request, key string) {
	switch r.Method {
	case http.MethodGet:
		u, ok := s.lookup(key)
		if !ok {
			pkrkit.JSON(w, http.StatusNotFound, map[string]any{"error": "unknown upstream key: " + key})
			return
		}
		pkrkit.JSON(w, http.StatusOK, map[string]any{"key": key, "url": u, "override": s.Registry.Upstreams.IsOverride(key)})
	case http.MethodPut:
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		if _, ok := s.lookup(key); !ok {
			pkrkit.JSON(w, http.StatusNotFound, map[string]any{"error": "unknown upstream key: " + key})
			return
		}
		var body struct {
			URL string `json:"url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.URL == "" {
			pkrkit.JSON(w, http.StatusBadRequest, map[string]any{"error": "missing url"})
			return
		}
		s.Registry.Upstreams.Set(key, body.URL)
		pkrkit.JSON(w, http.StatusOK, map[string]any{"key": key, "url": body.URL})
	case http.MethodDelete:
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		s.Registry.Upstreams.Reset(key)
		pkrkit.JSON(w, http.StatusOK, map[string]any{"key": key, "reset": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *State) listProxy(w http.ResponseWriter, r *http.Request) {
	pkrkit.JSON(w, http.StatusOK, map[string]any{"proxy": s.Registry.Upstreams.ProxyStates()})
}

func (s *State) proxyKey(w http.ResponseWriter, r *http.Request, key string) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := s.lookup(key); !ok {
			pkrkit.JSON(w, http.StatusNotFound, map[string]any{"error": "unknown upstream key: " + key})
			return
		}
		p, _ := s.Registry.Upstreams.ProxyURL(key)
		pkrkit.JSON(w, http.StatusOK, map[string]any{"key": key, "proxy": p})
	case http.MethodPut:
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		if _, ok := s.lookup(key); !ok {
			pkrkit.JSON(w, http.StatusNotFound, map[string]any{"error": "unknown upstream key: " + key})
			return
		}
		var body struct {
			Proxy string `json:"proxy"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.Registry.Upstreams.SetProxy(key, body.Proxy)
		p, _ := s.Registry.Upstreams.ProxyURL(key)
		pkrkit.JSON(w, http.StatusOK, map[string]any{"key": key, "proxy": p})
	case http.MethodDelete:
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		s.Registry.Upstreams.SetProxy(key, "")
		pkrkit.JSON(w, http.StatusOK, map[string]any{"key": key, "reset": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *State) packages(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		repo := r.URL.Query().Get("repo")
		if repo == "" {
			pkrkit.JSON(w, http.StatusBadRequest, map[string]any{"error": "missing repo"})
			return
		}
		n, err := s.Registry.Meta.DeleteRepo(r.Context(), "", repo)
		if err != nil {
			pkrkit.JSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		pkrkit.JSON(w, http.StatusOK, map[string]any{"repo": repo, "deleted": n})
		return
	}
	repo := r.URL.Query().Get("repo")
	pkgs, err := s.Registry.Meta.ListPackages(r.Context())
	if err != nil {
		pkrkit.JSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if repo != "" {
		var filtered []pkrkit.PackageSummary
		for _, p := range pkgs {
			if p.Repository == repo {
				filtered = append(filtered, p)
			}
		}
		pkgs = filtered
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"packages": pkgs})
}

var _ = context.Background

func stringsTrimPrefix(s, p string) string {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):]
	}
	return s
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
