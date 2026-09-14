package main

import (
	"context"
	"net"
	"net/http"

	"github.com/easylab-platform/easyvcs/store"
)

// Tenancy resolution at the gateway boundary.
//
// Users are 1:1 with tenants, so the tenant of a request is fully determined
// by its credential: Authorization bearer token → user → tenant. Anonymous
// callers (and open single-user instances) resolve to the default tenant (1),
// which keeps the pre-tenancy behavior byte-for-byte.

// tenantOfHeader resolves the caller's tenant from an Authorization bearer
// header. Unknown credentials and anonymous requests map to the default
// tenant; credentials are validated separately by the auth layer.
func (s *server) tenantOfHeader(h http.Header) int64 {
	if s.cs == nil {
		return 1
	}
	token, ok := credential(h)
	if !ok {
		return 1
	}
	return s.tenantOfToken(token)
}

// tenantOfRequest resolves the tenant of an inbound request. In addition to
// the bearer path, EMBEDDED extensions (same process, loopback) may tag
// their calls with X-Agent-Tenant: the agent envelope carries the tenant of
// every tool call / lifecycle event, and repo/sandbox operations must land
// in that tenant rather than the extension's own service identity. The
// header is only honored from loopback — it is a trust boundary, not an
// external API.
func (s *server) tenantOfRequest(r *http.Request) int64 {
	if t := r.Header.Get("X-Agent-Tenant"); t != "" && isLoopback(r.RemoteAddr) {
		if s.cs != nil {
			if tenant, err := s.cs.GetTenantBySlug(t); err == nil && !tenant.Disabled && tenant.ID != 0 {
				return tenant.ID
			}
		}
	}
	return s.tenantOfHeader(r.Header)
}

// isLoopback reports whether the peer address is this host.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// tenantOfToken resolves a bearer token's tenant (token → user → tenant).
func (s *server) tenantOfToken(token string) int64 {
	t, err := s.cs.LookupToken(token)
	if err != nil {
		return 1
	}
	u, err := s.cs.GetUser(t.UserID)
	if err != nil || u.TenantID == 0 {
		return 1
	}
	return u.TenantID
}

// repoRef builds the caller-scoped RepoRef for (org, repo): every Lab surface
// must go through here so no handler can accidentally operate cross-tenant.
func (s *server) repoRef(h http.Header, org, repo string) store.RepoRef {
	return store.RepoRef{Tenant: s.tenantOfHeader(h), Namespace: org, Name: repo}
}

// repoRefR is the *http.Request form (honors the embedded-extension tenant
// header).
func (s *server) repoRefR(r *http.Request, org, repo string) store.RepoRef {
	return store.RepoRef{Tenant: s.tenantOfRequest(r), Namespace: org, Name: repo}
}

// ---- request-scoped tenant on context (helpers that don't receive headers) ----

type tenantCtxKey struct{}

// withTenant attaches the caller's tenant to a context for downstream helpers
// that resolve repos without carrying the http.Header.
func withTenant(ctx context.Context, tid int64) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tid)
}

// tenantFromContext returns the request's tenant (default 1 when unset).
func tenantFromContext(ctx context.Context) int64 {
	if v, ok := ctx.Value(tenantCtxKey{}).(int64); ok && v != 0 {
		return v
	}
	return 1
}

// refsTenantOf is the refs() helper's accessor for the context tenant.
func refsTenantOf(ctx context.Context) int64 { return tenantFromContext(ctx) }

// agentTenantMiddleware attaches the caller's tenant to the request context
// for the agent.v1 forwarding surface.
func (s *server) agentTenantMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid := s.tenantOfHeader(r.Header)
		next.ServeHTTP(w, r.WithContext(withAgentTenant(r.Context(), tid)))
	})
}

// tenantCtx wraps a Connect handler mount with one request-scoped tenant
// resolution (bearer, plus the trusted loopback X-Agent-Tenant of embedded
// extensions); handlers read it via tenantFromContext.
func (s *server) tenantCtx(path string, handler http.Handler) (string, http.Handler) {
	return path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next := r.WithContext(withTenant(r.Context(), s.tenantOfRequest(r)))
		handler.ServeHTTP(w, next)
	})
}
