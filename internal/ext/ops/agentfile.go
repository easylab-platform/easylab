package opsext

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/agent-sdk-go/agent/v1"
)

// fetchAgentFile reads a stored file's bytes through the agent's GetFile RPC
// (the gateway forwards agent.v1 to the real agent). Files are OWNED by the
// agent: it resolves the caller's tenant from the forwarded credential and
// reads from whatever blob backend is configured (NATS object store or S3).
// The extension therefore never touches the shared store directly and needs
// no object-store credentials of its own.
//
// Tenant: the caller tagges the context with [ext.WithLabTenant]; the easylab
// client interceptor stamps X-Agent-Tenant (trusted on loopback), which the
// gateway maps to the user's bound agent credential before forwarding.
func (s *server) fetchAgentFile(ctx context.Context, code string) ([]byte, error) {
	if s.sdk == nil || s.sdk.Agent == nil {
		return nil, fmt.Errorf("agent file client unavailable")
	}
	res, err := s.sdk.Agent.GetFile(ctx, connect.NewRequest(&agentv1.GetFileRequest{Code: code}))
	if err != nil {
		return nil, fmt.Errorf("agent GetFile %q: %w", code, err)
	}
	data := res.Msg.GetData()
	if data == nil {
		return nil, fmt.Errorf("file not found: %s", code)
	}
	return data, nil
}
