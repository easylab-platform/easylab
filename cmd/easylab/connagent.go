package main

import (
	"context"
	"net/http"

	"connectrpc.com/connect"

	agentv1 "github.com/abcp-sdk/agent-proto/agent/v1"

	"github.com/abcp-sdk/agent-proto/agent/v1/agentv1connect"
)

// connAgent is the easylab-side gateway implementation of the agent.v1
// AgentService contract. It forwards every RPC to a real agent backend using
// the generated agent Connect client. Web frontends / Flutter connect to
// easylab (single entry) and never talk to the agent directly; ext servers
// connect to the agent directly.
type connAgent struct {
	client agentv1connect.AgentServiceClient
}

// h2cClient speaks unencrypted HTTP/2 (prior knowledge): the agent serves
// RPC/REST exclusively over HTTP/2, so plain HTTP/1.1 clients cannot talk to
// it. Go only enables h2 over TLS by default; the Protocols knob opts the
// transport into h2c on plain http:// URLs.
func h2cClient() *http.Client {
	protocols := new(http.Protocols)
	protocols.SetHTTP1(false)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Client{
		Transport: &http.Transport{Protocols: protocols},
	}
}

func newConnAgent(s *server, baseURL string) *connAgent {
	return &connAgent{
		client: agentv1connect.NewAgentServiceClient(
			h2cClient(),
			baseURL,
			connect.WithInterceptors(&tenantAgentBearer{s: s}),
		),
	}
}

// tenantAgentBearer picks the agent credential per REQUEST: the caller's
// tenant was attached to the request context by the agent.v1 mounting
// middleware (agentTenantMiddleware), and each tenant forwards with its own
// bootstrap token. The default tenant uses the deployment-wide
// EASYLAB_AGENT_TOKEN (phase-2 compatibility); a tenant without a stored
// credential sends none and the agent rejects the call — fail closed.
type tenantAgentBearer struct{ s *server }

func (b *tenantAgentBearer) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if tok := b.s.agentTokenForTenant(agentTenantOf(ctx)); tok != "" {
			req.Header().Set("Authorization", "Bearer "+tok)
		}
		return next(ctx, req)
	}
}

func (b *tenantAgentBearer) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		conn := next(ctx, spec)
		if tok := b.s.agentTokenForTenant(agentTenantOf(ctx)); tok != "" {
			conn.RequestHeader().Set("Authorization", "Bearer "+tok)
		}
		return conn
	}
}

func (b *tenantAgentBearer) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

var _ agentv1connect.AgentServiceHandler = (*connAgent)(nil)

// fwdReq builds a FRESH outbound request for forwarding: the inbound request's
// protocol-level headers (content-type, connect-protocol-version,
// connect-timeout-ms, grpc-timeout, compression negotiation, te, ...) must NOT
// ride through — the outbound client sets its own, and a duplicate makes
// downstream servers reject the call (a forwarded + self-set connect-timeout-ms
// produced "invalid connect timeout value: 29980, 29981" and a bare 415 on
// streaming RPCs). Application headers the gateway wants to pass (auth) are
// attached by the client interceptor, not by copying inbound headers.
func fwdReq[T any](req *connect.Request[T]) *connect.Request[T] {
	return connect.NewRequest(req.Msg)
}

func (c *connAgent) Health(ctx context.Context, req *connect.Request[agentv1.HealthRequest]) (*connect.Response[agentv1.HealthResponse], error) {
	return c.client.Health(ctx, fwdReq(req))
}
func (c *connAgent) ListSessions(ctx context.Context, req *connect.Request[agentv1.ListSessionsRequest]) (*connect.Response[agentv1.ListSessionsResponse], error) {
	return c.client.ListSessions(ctx, fwdReq(req))
}
func (c *connAgent) CreateSession(ctx context.Context, req *connect.Request[agentv1.CreateSessionRequest]) (*connect.Response[agentv1.CreateSessionResponse], error) {
	return c.client.CreateSession(ctx, fwdReq(req))
}
func (c *connAgent) GetSession(ctx context.Context, req *connect.Request[agentv1.GetSessionRequest]) (*connect.Response[agentv1.GetSessionResponse], error) {
	return c.client.GetSession(ctx, fwdReq(req))
}
func (c *connAgent) DeleteSession(ctx context.Context, req *connect.Request[agentv1.DeleteSessionRequest]) (*connect.Response[agentv1.DeleteSessionResponse], error) {
	return c.client.DeleteSession(ctx, fwdReq(req))
}
func (c *connAgent) ListMessages(ctx context.Context, req *connect.Request[agentv1.ListMessagesRequest]) (*connect.Response[agentv1.ListMessagesResponse], error) {
	return c.client.ListMessages(ctx, fwdReq(req))
}
func (c *connAgent) Prompt(ctx context.Context, req *connect.Request[agentv1.PromptRequest], srv *connect.ServerStream[agentv1.PromptResponse]) error {
	stream, err := c.client.Prompt(ctx, fwdReq(req))
	if err != nil {
		return err
	}
	for stream.Receive() {
		if err := srv.Send(stream.Msg()); err != nil {
			return err
		}
	}
	return stream.Err()
}
func (c *connAgent) WatchSession(ctx context.Context, req *connect.Request[agentv1.WatchSessionRequest], srv *connect.ServerStream[agentv1.WatchSessionResponse]) error {
	stream, err := c.client.WatchSession(ctx, fwdReq(req))
	if err != nil {
		return err
	}
	for stream.Receive() {
		if err := srv.Send(stream.Msg()); err != nil {
			return err
		}
	}
	return stream.Err()
}

func (c *connAgent) WatchSessions(ctx context.Context, req *connect.Request[agentv1.WatchSessionsRequest], srv *connect.ServerStream[agentv1.WatchSessionsResponse]) error {
	stream, err := c.client.WatchSessions(ctx, fwdReq(req))
	if err != nil {
		return err
	}
	for stream.Receive() {
		if err := srv.Send(stream.Msg()); err != nil {
			return err
		}
	}
	return stream.Err()
}

func (c *connAgent) Fork(ctx context.Context, req *connect.Request[agentv1.ForkRequest]) (*connect.Response[agentv1.ForkResponse], error) {
	return c.client.Fork(ctx, fwdReq(req))
}
func (c *connAgent) Rename(ctx context.Context, req *connect.Request[agentv1.RenameRequest]) (*connect.Response[agentv1.RenameResponse], error) {
	return c.client.Rename(ctx, fwdReq(req))
}
func (c *connAgent) SetModel(ctx context.Context, req *connect.Request[agentv1.SetModelRequest]) (*connect.Response[agentv1.SetModelResponse], error) {
	return c.client.SetModel(ctx, fwdReq(req))
}
func (c *connAgent) Undo(ctx context.Context, req *connect.Request[agentv1.UndoRequest]) (*connect.Response[agentv1.UndoResponse], error) {
	return c.client.Undo(ctx, fwdReq(req))
}
func (c *connAgent) State(ctx context.Context, req *connect.Request[agentv1.StateRequest]) (*connect.Response[agentv1.StateResponse], error) {
	return c.client.State(ctx, fwdReq(req))
}
func (c *connAgent) Mailbox(ctx context.Context, req *connect.Request[agentv1.MailboxRequest]) (*connect.Response[agentv1.MailboxResponse], error) {
	return c.client.Mailbox(ctx, fwdReq(req))
}
func (c *connAgent) UpdateSettings(ctx context.Context, req *connect.Request[agentv1.UpdateSettingsRequest]) (*connect.Response[agentv1.UpdateSettingsResponse], error) {
	return c.client.UpdateSettings(ctx, fwdReq(req))
}
func (c *connAgent) Interrupt(ctx context.Context, req *connect.Request[agentv1.InterruptRequest]) (*connect.Response[agentv1.InterruptResponse], error) {
	return c.client.Interrupt(ctx, fwdReq(req))
}
func (c *connAgent) Compact(ctx context.Context, req *connect.Request[agentv1.CompactRequest]) (*connect.Response[agentv1.CompactResponse], error) {
	return c.client.Compact(ctx, fwdReq(req))
}
func (c *connAgent) ListProviders(ctx context.Context, req *connect.Request[agentv1.ListProvidersRequest]) (*connect.Response[agentv1.ListProvidersResponse], error) {
	return c.client.ListProviders(ctx, fwdReq(req))
}
func (c *connAgent) ListProvidersCatalog(ctx context.Context, req *connect.Request[agentv1.ListProvidersCatalogRequest]) (*connect.Response[agentv1.ListProvidersCatalogResponse], error) {
	return c.client.ListProvidersCatalog(ctx, fwdReq(req))
}
func (c *connAgent) RegisterProvider(ctx context.Context, req *connect.Request[agentv1.RegisterProviderRequest]) (*connect.Response[agentv1.RegisterProviderResponse], error) {
	return c.client.RegisterProvider(ctx, fwdReq(req))
}
func (c *connAgent) DeleteProvider(ctx context.Context, req *connect.Request[agentv1.DeleteProviderRequest]) (*connect.Response[agentv1.DeleteProviderResponse], error) {
	return c.client.DeleteProvider(ctx, fwdReq(req))
}
func (c *connAgent) TestProvider(ctx context.Context, req *connect.Request[agentv1.TestProviderRequest]) (*connect.Response[agentv1.TestProviderResponse], error) {
	return c.client.TestProvider(ctx, fwdReq(req))
}
func (c *connAgent) ListModels(ctx context.Context, req *connect.Request[agentv1.ListModelsRequest]) (*connect.Response[agentv1.ListModelsResponse], error) {
	return c.client.ListModels(ctx, fwdReq(req))
}
func (c *connAgent) ListPresets(ctx context.Context, req *connect.Request[agentv1.ListPresetsRequest]) (*connect.Response[agentv1.ListPresetsResponse], error) {
	return c.client.ListPresets(ctx, fwdReq(req))
}
func (c *connAgent) UpsertPreset(ctx context.Context, req *connect.Request[agentv1.UpsertPresetRequest]) (*connect.Response[agentv1.UpsertPresetResponse], error) {
	return c.client.UpsertPreset(ctx, fwdReq(req))
}
func (c *connAgent) DeletePreset(ctx context.Context, req *connect.Request[agentv1.DeletePresetRequest]) (*connect.Response[agentv1.DeletePresetResponse], error) {
	return c.client.DeletePreset(ctx, fwdReq(req))
}
func (c *connAgent) PreviewPreset(ctx context.Context, req *connect.Request[agentv1.PreviewPresetRequest]) (*connect.Response[agentv1.PreviewPresetResponse], error) {
	return c.client.PreviewPreset(ctx, fwdReq(req))
}
func (c *connAgent) GetConfig(ctx context.Context, req *connect.Request[agentv1.GetConfigRequest]) (*connect.Response[agentv1.GetConfigResponse], error) {
	return c.client.GetConfig(ctx, fwdReq(req))
}
func (c *connAgent) SetConfig(ctx context.Context, req *connect.Request[agentv1.SetConfigRequest]) (*connect.Response[agentv1.SetConfigResponse], error) {
	return c.client.SetConfig(ctx, fwdReq(req))
}
func (c *connAgent) ListTools(ctx context.Context, req *connect.Request[agentv1.ListToolsRequest]) (*connect.Response[agentv1.ListToolsResponse], error) {
	return c.client.ListTools(ctx, fwdReq(req))
}
func (c *connAgent) GetToolConfig(ctx context.Context, req *connect.Request[agentv1.GetToolConfigRequest]) (*connect.Response[agentv1.GetToolConfigResponse], error) {
	return c.client.GetToolConfig(ctx, fwdReq(req))
}
func (c *connAgent) SetToolConfig(ctx context.Context, req *connect.Request[agentv1.SetToolConfigRequest]) (*connect.Response[agentv1.SetToolConfigResponse], error) {
	return c.client.SetToolConfig(ctx, fwdReq(req))
}
func (c *connAgent) SetExtensionConfig(ctx context.Context, req *connect.Request[agentv1.SetExtensionConfigRequest]) (*connect.Response[agentv1.SetExtensionConfigResponse], error) {
	return c.client.SetExtensionConfig(ctx, fwdReq(req))
}
func (c *connAgent) UploadFile(ctx context.Context, req *connect.Request[agentv1.UploadFileRequest]) (*connect.Response[agentv1.UploadFileResponse], error) {
	return c.client.UploadFile(ctx, fwdReq(req))
}
func (c *connAgent) IngestFile(ctx context.Context, req *connect.Request[agentv1.IngestFileRequest]) (*connect.Response[agentv1.IngestFileResponse], error) {
	return c.client.IngestFile(ctx, fwdReq(req))
}
func (c *connAgent) GetFile(ctx context.Context, req *connect.Request[agentv1.GetFileRequest]) (*connect.Response[agentv1.GetFileResponse], error) {
	return c.client.GetFile(ctx, fwdReq(req))
}
func (c *connAgent) GetFileMeta(ctx context.Context, req *connect.Request[agentv1.GetFileMetaRequest]) (*connect.Response[agentv1.GetFileMetaResponse], error) {
	return c.client.GetFileMeta(ctx, fwdReq(req))
}
func (c *connAgent) GetAgentConfig(ctx context.Context, req *connect.Request[agentv1.GetAgentConfigRequest]) (*connect.Response[agentv1.GetAgentConfigResponse], error) {
	return c.client.GetAgentConfig(ctx, fwdReq(req))
}
func (c *connAgent) DiscoverGatewayModels(ctx context.Context, req *connect.Request[agentv1.DiscoverGatewayModelsRequest]) (*connect.Response[agentv1.DiscoverGatewayModelsResponse], error) {
	return c.client.DiscoverGatewayModels(ctx, fwdReq(req))
}
