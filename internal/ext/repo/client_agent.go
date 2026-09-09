package repoext

import (
	"context"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/agent-proto/agent/v1"
	"github.com/abcp-sdk/agent-proto/agent/v1/agentv1connect"
	agentsdk "github.com/abcp-sdk/agent-sdk-go"
)

// agentClient talks to the abc agent session API via the generated Connect
// client (built by the caller per the connectrpc convention). The workspace
// layer only READS session state (list) plus a few writes — creating a session
// when adopting an orphan branch, forking, and deleting. Conflict responses
// mean "already exists" and are treated as idempotent success.
type agentClient struct {
	base string
	svc  agentv1connect.AgentServiceClient
}

func newAgentClient(base string) *agentClient {
	hc := agentsdk.NewHTTPClient()
	return &agentClient{
		base: base,
		svc: agentsdk.NewAgentServiceClient(
			hc,
			base,
			connect.WithInterceptors(agentsdk.AuthInterceptor(envOr("AGENT_API_KEY", ""))),
		),
	}
}

// EnsureSession creates the session; already-exists is success.
func (c *agentClient) EnsureSession(ctx context.Context, name string) error {
	_, err := c.svc.CreateSession(ctx, connect.NewRequest(&agentv1.CreateSessionRequest{Name: name}))
	if err == nil {
		return nil
	}
	if connect.CodeOf(err) == connect.CodeAlreadyExists || connect.CodeOf(err) == connect.CodeInvalidArgument {
		return nil
	}
	return err
}

// ListSessions returns every session name.
func (c *agentClient) ListSessions(ctx context.Context) (map[string]bool, error) {
	res, err := c.svc.ListSessions(ctx, connect.NewRequest(&agentv1.ListSessionsRequest{}))
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, s := range res.Msg.GetSessions() {
		if s.GetName() != "" {
			out[s.GetName()] = true
		}
	}
	return out, nil
}

// GetSession returns the session row (nil when absent); tip_id feeds fork
// point pinning.
func (c *agentClient) GetSession(ctx context.Context, name string) (map[string]interface{}, error) {
	res, err := c.svc.GetSession(ctx, connect.NewRequest(&agentv1.GetSessionRequest{Id: name}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, nil
		}
		return nil, err
	}
	s := res.Msg.GetSession()
	if s == nil {
		return nil, nil
	}
	return map[string]interface{}{
		"name": s.GetName(), "model": s.GetModel(), "preset": s.GetPreset(),
		"tip_id": s.GetTipId(), "org": s.GetOrg(), "repo": s.GetRepo(), "branch": s.GetBranch(),
	}, nil
}

// ForkSession forks parentSID into `name`. messageID pins the fork (empty =
// from tip); preset overrides the forked role. Already-exists is success.
func (c *agentClient) ForkSession(ctx context.Context, parentSID, name, messageID, preset string) error {
	_, err := c.svc.Fork(ctx, connect.NewRequest(&agentv1.ForkRequest{
		Id: parentSID, Name: name, MessageId: messageID, Preset: preset,
	}))
	if err == nil {
		return nil
	}
	if connect.CodeOf(err) == connect.CodeAlreadyExists || connect.CodeOf(err) == connect.CodeInvalidArgument {
		return nil
	}
	return err
}

// DeleteSession removes the session. An absent session is idempotent success.
func (c *agentClient) DeleteSession(ctx context.Context, name string) error {
	_, err := c.svc.DeleteSession(ctx, connect.NewRequest(&agentv1.DeleteSessionRequest{Id: name}))
	if err == nil {
		return nil
	}
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil
	}
	return err
}

