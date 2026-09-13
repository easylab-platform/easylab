package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/agent-proto/agent/v1"
	"github.com/abcp-sdk/agent-proto/agent/v1/agentv1connect"
	"github.com/easylab-platform/easyvcs/store"
)

// Tenant administration: one endpoint provisions a tenant END TO END across
// every subsystem:
//
//	easyvcs   → tenants row + first (admin) user + write token
//	agent v2  → AdminService.CreateTenant + bootstrap token (stored on the
//	            tenant row; the gateway presents it on forwarded agent RPCs)
//
// Authorization: a write-level credential of the DEFAULT tenant (the
// bootstrap/operations tenant). Per-tenant admin APIs land with the
// TenantService Connect surface later; this REST form is the minimal
// operator entry point.

type tenantCreateReq struct {
	Slug         string `json:"slug"`
	DisplayName  string `json:"display_name"`
	AdminUser    string `json:"admin_username"`
	AdminDisplay string `json:"admin_display_name"`
}

var tenantSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,38}[a-z0-9]$`)

func (s *server) handleTenantCreate(w http.ResponseWriter, r *http.Request) {
	// Only default-tenant operators may provision tenants.
	if s.tenantOfHeader(r.Header) != 1 {
		writeErr(w, http.StatusForbidden, fmt.Errorf("tenant administration requires a default-tenant operator credential"))
		return
	}
	var req tenantCreateReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !tenantSlugRe.MatchString(req.Slug) {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("slug must match %s", tenantSlugRe.String()))
		return
	}
	if req.AdminUser == "" {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("admin_username required"))
		return
	}

	// 1. easyvcs tenant + admin user + credential.
	tenant, err := s.cs.CreateTenant(req.Slug, req.DisplayName)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	user, err := s.cs.CreateUserTenant(tenant.ID, req.AdminUser, req.AdminDisplay)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	plaintext, err := generateToken()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	tok, err := s.cs.CreateToken(plaintext, user.ID, "write")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// 2. Agent tenant (protocol v2): mint the tenant's agent credential.
	agentToken := ""
	if s.agentAdmin() != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := s.agentAdmin().CreateTenant(ctx, connect.NewRequest(&agentv1.CreateTenantRequest{
			Id:   req.Slug,
			Name: req.DisplayName,
		}))
		if err != nil {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("agent tenant create: %v", err))
			return
		}
		agentToken = res.Msg.GetToken()
		if err := s.cs.SetAgentToken(tenant.ID, agentToken); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"tenant": map[string]any{"id": tenant.ID, "slug": tenant.Slug},
		"user":   map[string]any{"id": user.ID, "username": user.Username},
		// The user token is returned exactly once (the store keeps a hash).
		"token":        tok.Token,
		"agent_tenant": req.Slug != "" && agentToken != "",
	})
}

// agentAdmin lazily builds the AdminService client (static operator token
// from the environment; nil when the agent admin surface is not configured —
// tenant creation then proceeds without an agent tenant).
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

func (s *server) mountTenants(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/tenants", s.requireAuth(s.handleTenantCreate))
}

// ---- shared plumbing ----

// agentTokenForTenant resolves the credential the gateway must present to
// the agent for a tenant's forwarded RPCs: the tenant's bootstrap token, or
// the deployment-wide EASYLAB_AGENT_TOKEN for the default tenant.
func (s *server) agentTokenForTenant(tid int64) string {
	if tid == 0 || tid == 1 {
		return s.agentFwdToken
	}
	if tok, err := s.cs.AgentToken(tid); err == nil && tok != "" {
		return tok
	}
	// A tenant without an agent credential cannot borrow another's: fail
	// closed with an empty token (the agent rejects unauthenticated calls).
	return ""
}

// withAgentTenant attaches the caller's tenant to the forwarding context so
// the client interceptor can pick the right credential.
type agentTenantKey struct{}

func withAgentTenant(ctx context.Context, tid int64) context.Context {
	return context.WithValue(ctx, agentTenantKey{}, tid)
}

func agentTenantOf(ctx context.Context) int64 {
	if v, ok := ctx.Value(agentTenantKey{}).(int64); ok {
		return v
	}
	return 1
}

var _ = store.ErrUsernameTaken // keep store import for future membership wiring

// generateToken mints a URL-safe opaque credential.
func generateToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "elt_" + base64.RawURLEncoding.EncodeToString(buf), nil
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
