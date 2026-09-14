package main

import (
	"net"
	"net/http"
	"strings"

	"github.com/easylab-platform/easyvcs/store"
)

// labResponse is the envelope for Lab REST responses where a top-level Ok field
// is convenient. Most endpoints return the raw payload directly.
type labResponse map[string]any

// labJSON / labErr are retained for the residual HTTP surface (git smart
// protocol handlers in main.go) and error responses.
func labJSON(w http.ResponseWriter, code int, v any) { writeJSON(w, code, v) }

func labErr(w http.ResponseWriter, code int, err error) { writeErr(w, code, err) }

// labPrincipal resolves the caller from the bearer token. It returns the user
// (nil for anonymous) and an access level ("write"/"read"). Write access is
// granted by presenting a valid credential; anonymous callers are read-only.
//
// A loopback caller may additionally assert X-Agent-Tenant: the embedded agent
// extensions forward the repository OWNER (a username, or the user's bound
// agent tenant id) so agent-driven repo/branch operations act as that owner.
// The header is trusted only from loopback.
func (s *server) labPrincipal(r *http.Request) (*store.User, string) {
	if token, ok := requestCredential(r); ok {
		if tok, err := s.cs.LookupToken(token); err == nil {
			u, err := s.cs.GetUser(tok.UserID)
			if err == nil && !u.Disabled {
				if owner := s.loopbackOwner(r); owner != nil {
					return owner, "write"
				}
				return u, "write"
			}
		}
	}
	return nil, "read"
}

// loopbackOwner resolves a trusted X-Agent-Tenant assertion from a loopback
// peer to a user (nil when absent/off-loopback/unknown).
func (s *server) loopbackOwner(r *http.Request) *store.User {
	v := strings.TrimSpace(r.Header.Get("X-Agent-Tenant"))
	if v == "" || !isLoopback(r.RemoteAddr) {
		return nil
	}
	if u, err := s.cs.GetUserByUsername(v); err == nil && !u.Disabled {
		return u
	}
	if u, err := s.cs.GetUserByAgentTenant(v); err == nil && !u.Disabled {
		return u
	}
	return nil
}

// isLoopback reports whether the peer address is this host.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	host = strings.Trim(host, "[]")
	return host == "" || host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// labRequireWrite wraps a handler requiring an authenticated principal with a
// repository capability derived from the request shape. Retained for the git
// smart-protocol write handlers registered in main.go.
func (s *server) labRequireWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.requireWriteAuth(w, r, func() {
			next(w, r)
		})
	}
}

// labRequireWriteMirrorControl is like labRequireWrite but permits targeting a
// mirror repository (control-plane refresh).
func (s *server) labRequireWriteMirrorControl(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.requireWriteAuth(w, r, func() { next(w, r) })
	}
}

// requireWriteAuth validates the principal and the repository capability the
// request needs, then calls fn.
func (s *server) requireWriteAuth(w http.ResponseWriter, r *http.Request, fn func()) {
	u, _ := s.labPrincipal(r)
	if u == nil {
		labErr(w, http.StatusUnauthorized, errUnauthorizedWrite)
		return
	}
	if err := s.authorizeRepoRequest(r, u); err != nil {
		labErr(w, http.StatusForbidden, err)
		return
	}
	fn()
}

// writeTargetsMirror reports whether the request targets a mirror repository at
// a content sub-path (mirror repos are read-only).
func (s *server) writeTargetsMirror(r *http.Request) bool {
	rr := store.RepoRef{Namespace: r.PathValue("namespace"), Name: r.PathValue("repo")}
	if rr.Namespace == "" {
		rr.Namespace = r.PathValue("ns")
	}
	if rr.Name == "" {
		rr.Name = r.PathValue("name")
	}
	if rr.Namespace == "" || rr.Name == "" {
		return false
	}
	repo, err := s.cs.OpenRepo(rr)
	if err != nil {
		return false
	}
	return repo.IsMirror()
}
