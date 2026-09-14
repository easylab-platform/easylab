package repoext

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/agent-proto/agent/v1"
	"github.com/abcp-sdk/agent-proto/agent/v1/agentv1connect"
	agentsdk "github.com/abcp-sdk/agent-sdk-go"
	"github.com/easylab-platform/easylab/internal/connectauth"
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
	return &agentClient{
		base: base,
		svc: agentsdk.NewAgentServiceClient(
			agentHTTPClient(),
			base,
			connect.WithInterceptors(connectauth.Bearer(envOr("AGENT_API_KEY", ""))),
		),
	}
}

// agentHTTPClient is the cleartext-HTTP/2 (prior knowledge) client matching the
// agent's HTTP/2-only listener. The consumer owns the transport.
func agentHTTPClient() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(false)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{Transport: &http.Transport{Protocols: protocols}}
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

// CreateRepoSession creates a repository-bound session (name "org:repo:branch")
// with an explicit preset. `group` is left empty here — the easylab gateway
// derives it ("org/repo") from the repo coordinates. already-exists is success.
func (c *agentClient) CreateRepoSession(ctx context.Context, org, repo, branch, preset string) error {
	name := namingSession(org, repo, branch)
	_, err := c.svc.CreateSession(ctx, connect.NewRequest(&agentv1.CreateSessionRequest{
		Name: name, Org: org, Repo: repo, Branch: branch, Preset: preset,
	}))
	if err == nil {
		return nil
	}
	if connect.CodeOf(err) == connect.CodeAlreadyExists || connect.CodeOf(err) == connect.CodeInvalidArgument {
		return nil
	}
	return err
}

// SetGroup sets a session's generic `group` key (used by the backfill to tag
// existing repository sessions with "org/repo"). Idempotent.
func (c *agentClient) SetGroup(ctx context.Context, sessionName, group string) error {
	g := group
	_, err := c.svc.UpdateSettings(ctx, connect.NewRequest(&agentv1.UpdateSettingsRequest{
		Id: sessionName, Group: &g,
	}))
	return err
}

// Prompt delivers a user prompt to a session and returns its message id. The
// prompt is asynchronous: the call returns as soon as the turn is accepted.
// Used to wake a freshly-created subsession with its task.
func (c *agentClient) Prompt(ctx context.Context, sessionName, prompt string) (string, error) {
	stream, err := c.svc.Prompt(ctx, connect.NewRequest(&agentv1.PromptRequest{Id: sessionName, Prompt: prompt}))
	if err != nil {
		return "", err
	}
	defer stream.Close()
	for stream.Receive() {
		if stream.Msg().GetEvent() == "accepted" {
			return stream.Msg().GetParams()["message_id"], nil
		}
	}
	if err := stream.Err(); err != nil {
		return "", err
	}
	return "", nil
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
		"group": s.GetGroup(),
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
