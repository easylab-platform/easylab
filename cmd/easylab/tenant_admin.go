package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
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

	res, err := s.provisionTenant(req.Slug, req.DisplayName, req.AdminUser, req.AdminDisplay)
	if err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"tenant":       map[string]any{"id": res["id"], "slug": res["slug"]},
		"user":         map[string]any{"username": res["username"]},
		"token":        res["token"],
		"agent_tenant": res["agent_tenant"],
	})
}

// provisionTenant runs the end-to-end tenant setup shared by the REST and
// Connect surfaces and returns plain values (id/slug/username/token/flags).
func (s *server) provisionTenant(slug, displayName, adminUser, adminDisplay string) (map[string]any, error) {
	if !tenantSlugRe.MatchString(slug) {
		return nil, fmt.Errorf("slug must match %s", tenantSlugRe.String())
	}
	if adminUser == "" {
		return nil, fmt.Errorf("admin_username required")
	}
	tenant, err := s.cs.CreateTenant(slug, displayName)
	if err != nil {
		return nil, err
	}
	user, err := s.cs.CreateUserTenant(tenant.ID, adminUser, adminDisplay)
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
	agentToken := ""
	if s.agentAdmin() != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		res, err := s.agentAdmin().CreateTenant(ctx, connect.NewRequest(&agentv1.CreateTenantRequest{
			Id:   slug,
			Name: displayName,
		}))
		if err != nil {
			return nil, fmt.Errorf("agent tenant create: %w", err)
		}
		agentToken = res.Msg.GetToken()
		if err := s.cs.SetAgentBinding(tenant.ID, slug, agentToken); err != nil {
			return nil, err
		}
	}
	return map[string]any{
		"id":           strconv.FormatInt(tenant.ID, 10),
		"slug":         tenant.Slug,
		"username":     user.Username,
		"token":        tok.Token,
		"agent_tenant": agentToken != "",
	}, nil
}

// statusFor maps store errors to HTTP status codes for the REST surface.
func statusFor(err error) int {
	if err == nil {
		return http.StatusOK
	}
	if errors.Is(err, store.ErrUsernameTaken) || errors.Is(err, store.ErrRepoExists) {
		return http.StatusConflict
	}
	return http.StatusBadRequest
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
	mux.HandleFunc("DELETE /api/v1/tenants/{slug}", s.requireAuth(s.handleTenantDelete))
}

// handleTenantDelete removes a tenant by slug (default-tenant operator only),
// cascading to the agent tenant. Errors from the local delete are fatal; an
// agent-side failure is surfaced but the local delete still proceeds only when
// the agent tenant did not exist — otherwise the caller retries.
func (s *server) handleTenantDelete(w http.ResponseWriter, r *http.Request) {
	if s.tenantOfHeader(r.Header) != 1 {
		writeErr(w, http.StatusForbidden, fmt.Errorf("tenant administration requires a default-tenant operator credential"))
		return
	}
	slug := r.PathValue("slug")
	t, err := s.cs.GetTenantBySlug(slug)
	if err != nil {
		writeErr(w, http.StatusNotFound, fmt.Errorf("tenant %q not found", slug))
		return
	}
	agentID := s.agentTenantForTenant(t.ID)
	agentDeleted := false
	if agentID != "" && s.agentAdmin() != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		if _, aerr := s.agentAdmin().DeleteTenant(ctx, connect.NewRequest(&agentv1.DeleteTenantRequest{Id: agentID})); aerr != nil {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("agent tenant delete: %w", aerr))
			return
		}
		agentDeleted = true
	}
	if err := s.cs.DeleteTenant(t.ID); err != nil {
		writeErr(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "agent_deleted": agentDeleted})
}

// ---- shared plumbing ----

// agentTokenForTenant resolves the credential the gateway must present to
// the agent for a tenant's forwarded RPCs. The binding is a DB record
// (tenants.agent_token); the deployment-wide EASYLAB_AGENT_TOKEN is a fallback
// for the default tenant only when the DB row is empty (an agent predating the
// binding, or a fresh database before bootstrapAgentTenant runs).
func (s *server) agentTokenForTenant(tid int64) string {
	if tid == 0 {
		tid = 1
	}
	if _, tok, err := s.cs.AgentBinding(tid); err == nil && tok != "" {
		return tok
	}
	if tid == 1 {
		return s.agentFwdToken
	}
	// A tenant without an agent credential cannot borrow another's: fail
	// closed with an empty token (the agent rejects unauthenticated calls).
	return ""
}

// agentTenantForTenant resolves the tenant's bound agent tenant id ("" when
// unbound; the default tenant falls back to EASYLAB_AGENT_TENANT / "default").
func (s *server) agentTenantForTenant(tid int64) string {
	if tid == 0 {
		tid = 1
	}
	if at, _, err := s.cs.AgentBinding(tid); err == nil && at != "" {
		return at
	}
	if tid == 1 {
		return envOrStr("EASYLAB_AGENT_TENANT", "default")
	}
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
