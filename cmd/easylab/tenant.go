package main

import (
	"context"
	"net/http"
	"strings"

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
	raw := h.Get("Authorization")
	token := strings.TrimPrefix(raw, "Bearer ")
	if token == "" || token == raw {
		return 1
	}
	return s.tenantOfToken(token)
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
