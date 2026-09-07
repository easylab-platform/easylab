package main

import (
	"context"

	agentsdk "forgejo.develop.10.199.64.20.nip.io/abc-protocol/agent-sdk"
)

// agentClient talks to the abc agent session API via the typed
// abc-protocol-agent-sdk. The workspace layer only READS session state (list)
// plus a few writes — creating a session when adopting an orphan branch,
// forking, and deleting. Conflict responses mean "already exists" and are
// treated as idempotent success.
type agentClient struct {
	base string
	sdk  *agentsdk.Client
}

func newAgentClient(base string) *agentClient {
	return &agentClient{base: base, sdk: agentsdk.New(base, envOr("AGENT_API_KEY", ""))}
}

// EnsureSession creates the session; already-exists is success.
func (c *agentClient) EnsureSession(ctx context.Context, name string) error {
	return c.sdk.EnsureSession(ctx, name)
}

// ListSessions returns every session name.
func (c *agentClient) ListSessions(ctx context.Context) (map[string]bool, error) {
	return c.sdk.ListSessions(ctx)
}

// GetSession returns the session row (nil when absent); tip_id feeds fork
// point pinning.
func (c *agentClient) GetSession(ctx context.Context, name string) (map[string]interface{}, error) {
	s, err := c.sdk.GetSession(ctx, name)
	if err != nil {
		return nil, err
	}
	if s == nil {
		return nil, nil
	}
	return map[string]interface{}{
		"name": s.Name, "model": s.Model, "preset": s.Preset,
		"tip_id": s.TipID, "org": s.Org, "repo": s.Repo, "branch": s.Branch,
	}, nil
}

// ForkSession forks parentSID into `name`. messageID pins the fork (empty =
// from tip); preset overrides the forked role. Already-exists is success.
func (c *agentClient) ForkSession(ctx context.Context, parentSID, name, messageID, preset string) error {
	return c.sdk.Fork(ctx, parentSID, name, messageID, preset)
}

// DeleteSession removes the session. An absent session is idempotent success.
func (c *agentClient) DeleteSession(ctx context.Context, name string) error {
	return c.sdk.DeleteSession(ctx, name)
}
