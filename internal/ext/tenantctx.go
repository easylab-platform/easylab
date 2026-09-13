package ext

import (
	"context"
	"net/http"

	"connectrpc.com/connect"
)

// Cross-extension tenancy context: the agent envelope carries the tenant of
// every tool call / lifecycle event; extensions attach it to their easylab
// client requests via the trusted loopback header X-Agent-Tenant so the
// gateway performs repo/registry/sandbox operations in the right tenant.

type labTenantKey struct{}

// WithLabTenant tags the context with the agent tenant for downstream
// easylab calls.
func WithLabTenant(ctx context.Context, tenant string) context.Context {
	if tenant == "" {
		return ctx
	}
	return context.WithValue(ctx, labTenantKey{}, tenant)
}

// LabTenantOf extracts the tenant tag ("" when unset).
func LabTenantOf(ctx context.Context) string {
	if v, ok := ctx.Value(labTenantKey{}).(string); ok {
		return v
	}
	return ""
}

// LabTenantInterceptor is a connect client interceptor that stamps
// X-Agent-Tenant from the context onto every easylab RPC.
func LabTenantInterceptor() connect.Interceptor {
	return &labTenant{}
}

type labTenant struct{}

func (l *labTenant) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if t := LabTenantOf(ctx); t != "" {
			req.Header().Set("X-Agent-Tenant", t)
		}
		return next(ctx, req)
	}
}

func (l *labTenant) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if t := LabTenantOf(ctx); t != "" {
			conn.RequestHeader().Set("X-Agent-Tenant", t)
		}
		return conn
	}
}

func (l *labTenant) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// LabTenantTransport stamps X-Agent-Tenant on plain HTTP (REST) calls.
type LabTenantTransport struct{ Next http.RoundTripper }

func (t LabTenantTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if v := LabTenantOf(r.Context()); v != "" {
		r = r.Clone(r.Context())
		r.Header.Set("X-Agent-Tenant", v)
	}
	return t.Next.RoundTrip(r)
}
