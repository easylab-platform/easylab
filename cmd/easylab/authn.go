package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"connectrpc.com/connect"

	"github.com/easylab-platform/easyvcs/store"
)

// Authentication + authorization for the easylab gateway.
//
// Identity: a user IS the ownership boundary (there is no separate tenant). A
// bearer credential resolves to exactly one user via the easyvcs token store.
// Admin operations (user administration) require EASYLAB_ADMIN_TOKEN.
//
// Authorization: every repository operation resolves the caller's effective
// role on the repo (owner / maintainer / developer / none) and compares it to
// the capability the RPC needs. This is the ONE authorization path shared by
// the REST and Connect surfaces.

// Principal is the authenticated caller of a request.
type Principal struct {
	UserID    int64
	Username  string
	Anonymous bool
	Admin     bool
}

// principalKey is the context key carrying the Principal.
type principalKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// principalOf returns the request principal (zero value = anonymous when the
// auth interceptor did not run, e.g. on an unmounted path).
func principalOf(ctx context.Context) Principal {
	if p, ok := ctx.Value(principalKey{}).(Principal); ok {
		return p
	}
	return Principal{Anonymous: true}
}

// userIDOf returns the principal's user id (0 for anonymous) for store calls.
func userIDOf(ctx context.Context) int64 { return principalOf(ctx).UserID }

// adminToken is the static credential for user administration.
func adminToken() string { return os.Getenv("EASYLAB_ADMIN_TOKEN") }

// authenticateCredential resolves a credential to a Principal. It returns
// (Principal{}, false) when the credential is present but invalid (fail
// closed); absence of a credential yields an anonymous principal.
func (s *server) authenticateCredential(token string) (Principal, bool) {
	if token == "" {
		return Principal{Anonymous: true}, true
	}
	if at := adminToken(); at != "" && token == at {
		return Principal{Admin: true, Username: "admin"}, true
	}
	tok, err := s.cs.LookupToken(token)
	if err != nil {
		return Principal{}, false
	}
	u, err := s.cs.GetUser(tok.UserID)
	if err != nil || u.Disabled {
		return Principal{}, false
	}
	return Principal{UserID: u.ID, Username: u.Username}, true
}

// authInterceptor is the server-side Connect interceptor: it authenticates
// every RPC (Health excepted) and attaches the Principal to the call context.
// Authorization is enforced per RPC by the handlers via repoRole/require*.
func (s *server) authInterceptor() connect.Interceptor {
	return &authnInterceptor{s: s}
}

type authnInterceptor struct{ s *server }

func (a *authnInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if isHealthRPC(req.Spec().Procedure) {
			return next(ctx, req)
		}
		p, ok := a.authenticate(req.Header())
		if !ok {
			return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid or expired credential"))
		}
		// Loopback callers may assert repository ownership via X-Agent-Tenant
		// (the embedded agent extensions forward the session's repo owner).
		p = a.applyLoopbackOverride(p, req.Header(), req.Peer())
		return next(withPrincipal(ctx, p), req)
	}
}

func (a *authnInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		p, ok := a.authenticate(conn.RequestHeader())
		if !ok {
			return connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("invalid or expired credential"))
		}
		return next(withPrincipal(ctx, p), conn)
	}
}

func (a *authnInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

// authenticate reads the credential from the request headers via the shared
// scheme helper and resolves it.
func (a *authnInterceptor) authenticate(h http.Header) (Principal, bool) {
	token, _ := credential(h)
	return a.s.authenticateCredential(token)
}

// applyLoopbackOverride honors a loopback-only X-Agent-Tenant header: the
// embedded agent extensions call the gateway from the same pod and forward the
// repository OWNER's identity (the users-as-tenants model names a user's agent
// tenant after the user). The value may be a username (preferred) or the bound
// agent tenant id. The header is ignored off loopback (trust boundary) and
// never widens an admin principal.
func (a *authnInterceptor) applyLoopbackOverride(p Principal, h http.Header, peer connect.Peer) Principal {
	if p.Admin {
		return p
	}
	owner := strings.TrimSpace(h.Get("X-Agent-Tenant"))
	if owner == "" || !isLoopbackPeer(peer) {
		return p
	}
	if u, err := a.s.cs.GetUserByUsername(owner); err == nil && !u.Disabled {
		p.UserID = u.ID
		p.Username = u.Username
		return p
	}
	if u, err := a.s.cs.GetUserByAgentTenant(owner); err == nil && !u.Disabled {
		p.UserID = u.ID
		p.Username = u.Username
	}
	return p
}

// isLoopbackPeer reports whether the Connect peer is this host.
func isLoopbackPeer(peer connect.Peer) bool {
	host := peer.Addr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "" || host == "127.0.0.1" || host == "::1" || host == "localhost"
}

func isHealthRPC(procedure string) bool {
	return strings.HasSuffix(procedure, "/Health")
}

// Capability predicates passed to requireRepoAction / authorizeRepo. They are
// named values so call sites read as English ("needs repoCanPush").
var (
	repoCanRead    = func(r store.Role) bool { return r.CanRead() }
	repoCanPropose = func(r store.Role) bool { return r.CanPropose() }
	repoCanMerge   = func(r store.Role) bool { return r.CanMerge() }
	repoCanPush    = func(r store.Role) bool { return r.CanPush() }
)

// ---- repository authorization ----

// repoRole resolves the caller's effective role on (namespace, name).
func (s *server) repoRole(ctx context.Context, namespace, name string) (store.Role, error) {
	ref := store.RepoRef{Namespace: namespace, Name: name}
	return s.cs.RoleOfRef(ref, userIDOf(ctx))
}

// canReadRepo reports whether the caller may read the repo (used by the REST
// read path; Connect handlers call repoRole directly).
func (s *server) canReadRepo(ctx context.Context, namespace, name string) bool {
	role, err := s.repoRole(ctx, namespace, name)
	return err == nil && role.CanRead()
}

// requireRepoAction returns a PermissionDenied connect error when the caller's
// role does not satisfy the predicate. It is the single point every repository
// RPC funnels through.
func (s *server) requireRepoAction(ctx context.Context, namespace, name string, ok func(store.Role) bool, action string) error {
	role, err := s.repoRole(ctx, namespace, name)
	if err != nil {
		return connect.NewError(connect.CodeNotFound, err)
	}
	if !ok(role) {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s requires a stronger role on %s/%s (have %q)", action, namespace, name, role))
	}
	return nil
}

// openRepoAuthorized opens namespace/name and enforces that the caller's role
// satisfies prop. It is the repository accessor used by every data-plane RPC.
func (s *server) openRepoAuthorized(ctx context.Context, namespace, name string, prop func(store.Role) bool, action string) (*store.Repo, error) {
	if err := s.requireRepoAction(ctx, namespace, name, prop, action); err != nil {
		return nil, err
	}
	repo, err := s.cs.OpenRepo(store.RepoRef{Namespace: namespace, Name: name})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return repo, nil
}

// requireAuthenticated returns a PermissionDenied error for anonymous callers.
func requireAuthenticated(ctx context.Context, action string) error {
	if principalOf(ctx).Anonymous {
		return connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("%s requires authentication", action))
	}
	return nil
}

// requireAdmin returns a PermissionDenied error unless the caller presented the
// admin credential.
func requireAdmin(ctx context.Context, action string) error {
	if !principalOf(ctx).Admin {
		return connect.NewError(connect.CodePermissionDenied, fmt.Errorf("%s requires the admin credential", action))
	}
	return nil
}

// authorizeSession enforces access to an agent session. A repository-bound
// session is named "org:repo:branch": only the repository OWNER may drive it
// (the session drives that repo/branch). Any other session belongs to the
// caller's own agent identity, so authentication is sufficient.
func (s *server) authorizeSession(ctx context.Context, sessionID string) error {
	if err := requireAuthenticated(ctx, "accessing a session"); err != nil {
		return err
	}
	parts := strings.Split(sessionID, ":")
	if len(parts) != 3 {
		return nil // non-repo session: owned by the caller's agent identity
	}
	org, repo := parts[0], parts[1]
	return s.requireRepoAction(ctx, org, repo, repoCanPush, "accessing a repository session")
}

// ---- REST authorization (legacy /api/v1 + git-smart surface) ----

// authorizeRepo enforces a repository capability for a REST actor. It is the
// REST twin of requireRepoAction: callers pass the authenticated user, the
// repo coordinates, and the required capability predicate.
func (s *server) authorizeRepo(r *http.Request, u *store.User, namespace, name string, prop func(store.Role) bool, action string) error {
	if u == nil {
		return fmt.Errorf("unauthorized: %s requires a token", action)
	}
	role, err := s.cs.RoleOfRef(store.RepoRef{Namespace: namespace, Name: name}, u.ID)
	if err != nil {
		return fmt.Errorf("repository %s/%s not found", namespace, name)
	}
	if !prop(role) {
		return fmt.Errorf("%s requires a stronger role on %s/%s (have %q)", action, namespace, name, role)
	}
	return nil
}

// repoCapForRequest maps a legacy REST/git request to the repository capability
// it needs. Unknown shapes default to CanPush (owner) so a new endpoint can
// never silently under-authorize.
func (s *server) repoCapForRequest(r *http.Request) (func(store.Role) bool, string) {
	p := r.URL.Path
	m := r.Method
	isRead := m == http.MethodGet || m == http.MethodHead
	if isRead {
		return repoCanRead, "reading"
	}
	// Maintainer actions: merge requests + releases + tags.
	switch {
	case strings.Contains(p, "/merge_requests/") && strings.HasSuffix(p, "/merge"):
		return repoCanMerge, "merging a change request"
	case strings.Contains(p, "/merge_requests/") && (strings.HasSuffix(p, "/reviews") || strings.HasSuffix(p, "/comments")):
		return repoCanPropose, "reviewing a change request"
	case strings.Contains(p, "/merge_requests/"):
		return repoCanMerge, "updating a change request"
	case strings.Contains(p, "/merge_requests"):
		return repoCanPropose, "opening a change request"
	case strings.Contains(p, "/tags"):
		return repoCanMerge, "managing tags"
	case strings.Contains(p, "/releases"):
		return repoCanMerge, "managing releases"
	}
	// Everything else that writes a branch/repo is owner-only.
	return repoCanPush, "writing"
}

// authorizeRepoRequest is the REST authorization gate used by labRequireWrite:
// when the route addresses a specific repository (path params namespace+repo/
// name) it derives the needed capability from the request shape and enforces
// the caller's repo role; otherwise (create-repo, user/token admin, ...) it is
// authentication-only, which labRequireWrite has already established.
func (s *server) authorizeRepoRequest(r *http.Request, u *store.User) error {
	ns := r.PathValue("namespace")
	name := r.PathValue("repo")
	if name == "" {
		name = r.PathValue("name")
	}
	if ns == "" {
		ns = r.PathValue("ns")
	}
	if ns == "" || name == "" {
		return nil // not a repo-scoped route: authentication is sufficient
	}
	prop, action := s.repoCapForRequest(r)
	return s.authorizeRepo(r, u, ns, name, prop, action)
}
