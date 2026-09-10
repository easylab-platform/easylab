// Package connectauth provides a bearer-token Connect interceptor that works
// for BOTH unary and streaming RPCs.
//
// connect.UnaryInterceptorFunc is a documented no-op for streams, so a
// unary-only interceptor silently omits Authorization on server-streaming
// calls (e.g. WatchJob, RunJobLog) — which a fail-closed server then rejects.
// This interceptor implements WrapStreamingClient too, so the header is set on
// every outbound request.
package connectauth

import (
	"context"

	"connectrpc.com/connect"
)

// Bearer returns a Connect interceptor that sets `Authorization: Bearer <token>`
// on every outbound request (unary and streaming). An empty token is a no-op.
func Bearer(token string) connect.Interceptor { return &bearer{token: token} }

type bearer struct{ token string }

func (b *bearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if b.token != "" {
			req.Header().Set("Authorization", "Bearer "+b.token)
		}
		return next(ctx, req)
	}
}

func (b *bearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if b.token != "" {
			conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		}
		return conn
	}
}

// WrapStreamingHandler is a no-op: this interceptor is for clients.
func (b *bearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
