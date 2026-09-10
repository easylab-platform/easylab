// Package easylabclient builds the easylab gateway Connect clients.
//
// The easylab-sdk-go package ships ONLY generated code (easylab.v1 +
// worker.v1). Per the connectrpc convention this consumer owns its transport:
// an HTTP/2-only client (cleartext h2c for http://, ALPN for https://) plus a
// bearer-auth interceptor.
package easylabclient

import (
	"context"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/abcp-sdk/agent-proto/agent/v1/agentv1connect"
	"github.com/easylab-platform/easylab-proto/easylab/v1/easylabv1connect"
)

// Services bundles every easylab gateway Connect client. The gateway serves
// easylab.v1 (lab/ops/registry/sandbox/workflow) and forwards agent.v1.
type Services struct {
	Lab      easylabv1connect.LabServiceClient
	Ops      easylabv1connect.OpsServiceClient
	Registry easylabv1connect.RegistryServiceClient
	Sandbox  easylabv1connect.SandboxServiceClient
	Workflow easylabv1connect.WorkflowServiceClient
	Agent    agentv1connect.AgentServiceClient
}

// h2cTransport speaks cleartext HTTP/2 (prior knowledge) for http:// URLs and
// TLS HTTP/2 (ALPN) for https:// — one HTTP/2-only transport. The gateway
// serves the Connect surface over HTTP/2.
func h2cTransport() *http.Transport {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(false)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Transport{Protocols: protocols}
}

// bearerInterceptor attaches `Authorization: Bearer <token>` to every request,
// unary AND streaming (server-streaming RPCs like RunJobLog must carry it too;
// connect.UnaryInterceptorFunc is a no-op for streams).
func bearerInterceptor(token string) connect.Interceptor {
	return &authInterceptor{token: token}
}

type authInterceptor struct{ token string }

func (a *authInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if a.token != "" {
			req.Header().Set("Authorization", "Bearer "+a.token)
		}
		return next(ctx, req)
	}
}

func (a *authInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if a.token != "" {
			conn.RequestHeader().Set("Authorization", "Bearer "+a.token)
		}
		return conn
	}
}

func (a *authInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func trimSlash(s string) string {
	return strings.TrimRight(s, "/")
}

// New builds the gateway clients. When token is empty the default "devtoken"
// is used (matching the rest of the stack).
func New(baseURL, token string) *Services {
	if token == "" {
		token = "devtoken"
	}
	hc := &http.Client{Transport: h2cTransport()}
	base := trimSlash(baseURL)
	opts := []connect.ClientOption{connect.WithInterceptors(bearerInterceptor(token))}
	return &Services{
		Lab:      easylabv1connect.NewLabServiceClient(hc, base, opts...),
		Ops:      easylabv1connect.NewOpsServiceClient(hc, base, opts...),
		Registry: easylabv1connect.NewRegistryServiceClient(hc, base, opts...),
		Sandbox:  easylabv1connect.NewSandboxServiceClient(hc, base, opts...),
		Workflow: easylabv1connect.NewWorkflowServiceClient(hc, base, opts...),
		Agent:    agentv1connect.NewAgentServiceClient(hc, base, opts...),
	}
}
