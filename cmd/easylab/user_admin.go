package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/agent-proto/agent/v1"
	"github.com/abcp-sdk/agent-proto/agent/v1/agentv1connect"
)

// User administration: one call provisions a user END TO END across every
// subsystem:
//
//	easyvcs   → users row + write token
//	agent v2  → AdminService.CreateTenant(id=username) + bootstrap token
//	            (stored on the user; the gateway presents it on forwarded
//	            agent RPCs)
//
// Authorization: EASYLAB_ADMIN_TOKEN (the static admin credential). A user IS
// the ownership boundary, so each user gets their own agent tenant named after
// their username.

// generateToken mints a URL-safe opaque credential.
func generateToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "elt_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// provisionUser runs the end-to-end user setup shared by every admin surface
// and returns plain values (id/username/token/agent flag).
func (s *server) provisionUser(username, displayName string) (map[string]any, error) {
	if username == "" {
		return nil, fmt.Errorf("username required")
	}
	user, err := s.cs.CreateUser(username, displayName)
	if err != nil {
		return nil, err
	}
	plaintext, err := generateToken()
	if err != nil {
		return nil, err
	}
	tok, err := s.cs.CreateToken(plaintext, user.ID, "write")
	if err != nil {
		return nil, err
	}
	agentTenant := ""
	if s.agentAdmin() != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := s.agentAdmin().CreateTenant(ctx, connect.NewRequest(&agentv1.CreateTenantRequest{
			Id:   username,
			Name: displayName,
		}))
		if err != nil {
			return nil, fmt.Errorf("agent tenant create: %w", err)
		}
		agentTenant = username
		if err := s.cs.SetAgentBinding(user.ID, username, res.Msg.GetToken()); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"id":           user.ID,
		"username":     user.Username,
		"token":        tok.Token,
		"agent_tenant": agentTenant,
	}, nil
}

// agentAdmin lazily builds the AdminService client (static operator token from
// the environment; nil when the agent admin surface is not configured — user
// provisioning then proceeds without an agent tenant).
func (s *server) agentAdmin() agentv1connect.AdminServiceClient {
	s.agentAdminOnce.Do(func() {
		url := s.agentAdminURL
		token := s.agentAdminToken
		if url == "" || token == "" {
			return
		}
		s.agentAdminClient = agentv1connect.NewAdminServiceClient(h2cClient(), url, connect.WithInterceptors(&staticBearer{token: token}))
	})
	return s.agentAdminClient
}

// agentTokenForUser resolves the credential the gateway presents to the agent
// for a user's forwarded RPCs: the user's DB binding (users.agent_token), with
// the deployment-wide EASYLAB_AGENT_TOKEN as fallback when the user has no
// binding (single-user / bootstrap deployments).
func (s *server) agentTokenForUser(userID int64) string {
	if userID != 0 {
		if _, tok, err := s.cs.AgentBinding(userID); err == nil && tok != "" {
			return tok
		}
	}
	return s.agentFwdToken
}

// staticBearer attaches a fixed operator credential (agent AdminService).
type staticBearer struct{ token string }

func (b *staticBearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if b.token != "" {
			req.Header().Set("Authorization", "Bearer "+b.token)
		}
		return next(ctx, req)
	}
}

func (b *staticBearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if b.token != "" {
			conn.RequestHeader().Set("Authorization", "Bearer "+b.token)
		}
		return conn
	}
}

func (b *staticBearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}
