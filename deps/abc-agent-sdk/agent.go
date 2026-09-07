// Package agentsdk provides a strong-typed client for the abc AgentService.
//
// The abc agent is a shared backend. ext servers (and the easylab gateway)
// use this SDK to talk to it over Connect (agent.v1.AgentService). Web
// frontends and Flutter never connect to the agent directly — they go through
// easylab, which forwards this same surface. The underlying RPC client is
// generated from agent/v1/agent.proto by buf; this package adds auth + a
// few ergonomic helpers.
package agentsdk

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/agent-proto/agent/v1"
	"github.com/abcp-sdk/agent-proto/agent/v1/agentv1connect"
)

// Client is a thin, status-aware agent session/file client.
type Client struct {
	base string
	svc  agentv1connect.AgentServiceClient
}

// New builds an agent client. token, when non-empty, is sent as a Bearer
// header on every request. baseURL is protocol+host (no trailing slash).
func New(baseURL, token string) *Client {
	if token == "" {
		token = "devtoken"
	}
	return &Client{
		base: baseURL,
		svc: agentv1connect.NewAgentServiceClient(
			http.DefaultClient,
			baseURL,
			connect.WithInterceptors(authInterceptor(token)),
		),
	}
}

func authInterceptor(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})
}

// ---- session ----

// Session mirrors the agent's session row.
type Session struct {
	Name        string
	Model       string
	Preset      string
	TipID       string
	Org, Repo   string
	Branch      string
	SystemPrompt string
	MaxTurns    int32
}

func sessionFromPb(s *agentv1.Session) Session {
	if s == nil {
		return Session{}
	}
	return Session{
		Name:         s.GetName(),
		Model:        s.GetModel(),
		Preset:       s.GetPreset(),
		TipID:        s.GetTipId(),
		Org:          s.GetOrg(),
		Repo:         s.GetRepo(),
		Branch:       s.GetBranch(),
		SystemPrompt: s.GetSystemPrompt(),
		MaxTurns:     s.GetMaxTurns(),
	}
}

// EnsureSession creates the session; an existing session ("already exists",
// modelled as AlreadyExists/Conflict) is treated as idempotent success.
func (c *Client) EnsureSession(ctx context.Context, name string) error {
	_, err := c.svc.CreateSession(ctx, connect.NewRequest(&agentv1.CreateSessionRequest{Name: name}))
	if err == nil {
		return nil
	}
	if IsExists(err) {
		return nil
	}
	return errDownstream("agent", err)
}

// ListSessions returns every session name.
func (c *Client) ListSessions(ctx context.Context) (map[string]bool, error) {
	res, err := c.svc.ListSessions(ctx, connect.NewRequest(&agentv1.ListSessionsRequest{}))
	if err != nil {
		return nil, errDownstream("agent", err)
	}
	out := map[string]bool{}
	for _, s := range res.Msg.GetSessions() {
		if s.GetName() != "" {
			out[s.GetName()] = true
		}
	}
	return out, nil
}

// GetSession returns the session row (nil when absent).
func (c *Client) GetSession(ctx context.Context, name string) (*Session, error) {
	res, err := c.svc.GetSession(ctx, connect.NewRequest(&agentv1.GetSessionRequest{Id: name}))
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, errDownstream("agent", err)
	}
	s := sessionFromPb(res.Msg.GetSession())
	return &s, nil
}

// Fork pins a session fork. messageID empty forks from the tip; preset
// overrides the forked session's role. Existing name is idempotent success.
func (c *Client) Fork(ctx context.Context, parentSID, name, messageID, preset string) error {
	req := &agentv1.ForkRequest{Id: parentSID, Name: name, MessageId: messageID, Preset: preset}
	_, err := c.svc.Fork(ctx, connect.NewRequest(req))
	if err == nil || IsExists(err) {
		return nil
	}
	return errDownstream("agent", err)
}

// DeleteSession removes a session; an absent session is idempotent success.
func (c *Client) DeleteSession(ctx context.Context, name string) error {
	_, err := c.svc.DeleteSession(ctx, connect.NewRequest(&agentv1.DeleteSessionRequest{Id: name}))
	if err == nil || IsNotFound(err) {
		return nil
	}
	return errDownstream("agent", err)
}

// ---- file ----

// GetFile returns a stored file's bytes + metadata by code.
func (c *Client) GetFile(ctx context.Context, code string) (data []byte, name, mime string, err error) {
	res, rerr := c.svc.GetFile(ctx, connect.NewRequest(&agentv1.GetFileRequest{Code: code}))
	if rerr != nil {
		if IsNotFound(rerr) {
			return nil, "", "", ErrNotFound
		}
		return nil, "", "", errDownstream("agent", rerr)
	}
	return []byte(res.Msg.GetData()), res.Msg.GetName(), res.Msg.GetMime(), nil
}

// GetFileMeta returns a stored file's metadata by code.
func (c *Client) GetFileMeta(ctx context.Context, code string) (*agentv1.GetFileMetaResponse, error) {
	res, err := c.svc.GetFileMeta(ctx, connect.NewRequest(&agentv1.GetFileMetaRequest{Code: code}))
	if err != nil {
		return nil, errDownstream("agent", err)
	}
	return res.Msg, nil
}

// ---- errors ----

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

func IsNotFound(err error) bool {
	return connect.CodeOf(err) == connect.CodeNotFound
}
func IsExists(err error) bool {
	c := connect.CodeOf(err)
	return c == connect.CodeAlreadyExists || c == connect.CodeInvalidArgument
}

func errDownstream(svc string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", svc, err)
}
