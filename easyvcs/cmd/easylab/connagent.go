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

func newConnAgent(baseURL, token string) *connAgent {
	return &connAgent{
		client: agentv1connect.NewAgentServiceClient(
			http.DefaultClient,
			baseURL,
			connect.WithInterceptors(agentAuthInterceptor(token)),
		),
	}
}

func agentAuthInterceptor(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			return next(ctx, req)
		}
	})
}

var _ agentv1connect.AgentServiceHandler = (*connAgent)(nil)

func (c *connAgent) Health(ctx context.Context, req *connect.Request[agentv1.HealthRequest]) (*connect.Response[agentv1.HealthResponse], error) {
	return c.client.Health(ctx, req)
}
func (c *connAgent) ListSessions(ctx context.Context, req *connect.Request[agentv1.ListSessionsRequest]) (*connect.Response[agentv1.ListSessionsResponse], error) {
	return c.client.ListSessions(ctx, req)
}
func (c *connAgent) CreateSession(ctx context.Context, req *connect.Request[agentv1.CreateSessionRequest]) (*connect.Response[agentv1.CreateSessionResponse], error) {
	return c.client.CreateSession(ctx, req)
}
func (c *connAgent) GetSession(ctx context.Context, req *connect.Request[agentv1.GetSessionRequest]) (*connect.Response[agentv1.GetSessionResponse], error) {
	return c.client.GetSession(ctx, req)
}
func (c *connAgent) DeleteSession(ctx context.Context, req *connect.Request[agentv1.DeleteSessionRequest]) (*connect.Response[agentv1.DeleteSessionResponse], error) {
	return c.client.DeleteSession(ctx, req)
}
func (c *connAgent) ListMessages(ctx context.Context, req *connect.Request[agentv1.ListMessagesRequest]) (*connect.Response[agentv1.ListMessagesResponse], error) {
	return c.client.ListMessages(ctx, req)
}
func (c *connAgent) Prompt(ctx context.Context, req *connect.Request[agentv1.PromptRequest], srv *connect.ServerStream[agentv1.PromptResponse]) error {
	stream, err := c.client.Prompt(ctx, req)
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
	return c.client.Fork(ctx, req)
}
func (c *connAgent) Rename(ctx context.Context, req *connect.Request[agentv1.RenameRequest]) (*connect.Response[agentv1.RenameResponse], error) {
	return c.client.Rename(ctx, req)
}
func (c *connAgent) SetModel(ctx context.Context, req *connect.Request[agentv1.SetModelRequest]) (*connect.Response[agentv1.SetModelResponse], error) {
	return c.client.SetModel(ctx, req)
}
func (c *connAgent) Undo(ctx context.Context, req *connect.Request[agentv1.UndoRequest]) (*connect.Response[agentv1.UndoResponse], error) {
	return c.client.Undo(ctx, req)
}
func (c *connAgent) State(ctx context.Context, req *connect.Request[agentv1.StateRequest]) (*connect.Response[agentv1.StateResponse], error) {
	return c.client.State(ctx, req)
}
func (c *connAgent) Mailbox(ctx context.Context, req *connect.Request[agentv1.MailboxRequest]) (*connect.Response[agentv1.MailboxResponse], error) {
	return c.client.Mailbox(ctx, req)
}
func (c *connAgent) UpdateSettings(ctx context.Context, req *connect.Request[agentv1.UpdateSettingsRequest]) (*connect.Response[agentv1.UpdateSettingsResponse], error) {
	return c.client.UpdateSettings(ctx, req)
}
func (c *connAgent) Interrupt(ctx context.Context, req *connect.Request[agentv1.InterruptRequest]) (*connect.Response[agentv1.InterruptResponse], error) {
	return c.client.Interrupt(ctx, req)
}
func (c *connAgent) Compact(ctx context.Context, req *connect.Request[agentv1.CompactRequest]) (*connect.Response[agentv1.CompactResponse], error) {
	return c.client.Compact(ctx, req)
}
func (c *connAgent) ListProviders(ctx context.Context, req *connect.Request[agentv1.ListProvidersRequest]) (*connect.Response[agentv1.ListProvidersResponse], error) {
	return c.client.ListProviders(ctx, req)
}
func (c *connAgent) ListProvidersCatalog(ctx context.Context, req *connect.Request[agentv1.ListProvidersCatalogRequest]) (*connect.Response[agentv1.ListProvidersCatalogResponse], error) {
	return c.client.ListProvidersCatalog(ctx, req)
}
func (c *connAgent) RegisterProvider(ctx context.Context, req *connect.Request[agentv1.RegisterProviderRequest]) (*connect.Response[agentv1.RegisterProviderResponse], error) {
	return c.client.RegisterProvider(ctx, req)
}
func (c *connAgent) DeleteProvider(ctx context.Context, req *connect.Request[agentv1.DeleteProviderRequest]) (*connect.Response[agentv1.DeleteProviderResponse], error) {
	return c.client.DeleteProvider(ctx, req)
}
func (c *connAgent) TestProvider(ctx context.Context, req *connect.Request[agentv1.TestProviderRequest]) (*connect.Response[agentv1.TestProviderResponse], error) {
	return c.client.TestProvider(ctx, req)
}
func (c *connAgent) ListModels(ctx context.Context, req *connect.Request[agentv1.ListModelsRequest]) (*connect.Response[agentv1.ListModelsResponse], error) {
	return c.client.ListModels(ctx, req)
}
func (c *connAgent) ListPresets(ctx context.Context, req *connect.Request[agentv1.ListPresetsRequest]) (*connect.Response[agentv1.ListPresetsResponse], error) {
	return c.client.ListPresets(ctx, req)
}
func (c *connAgent) UpsertPreset(ctx context.Context, req *connect.Request[agentv1.UpsertPresetRequest]) (*connect.Response[agentv1.UpsertPresetResponse], error) {
	return c.client.UpsertPreset(ctx, req)
}
func (c *connAgent) DeletePreset(ctx context.Context, req *connect.Request[agentv1.DeletePresetRequest]) (*connect.Response[agentv1.DeletePresetResponse], error) {
	return c.client.DeletePreset(ctx, req)
}
func (c *connAgent) PreviewPreset(ctx context.Context, req *connect.Request[agentv1.PreviewPresetRequest]) (*connect.Response[agentv1.PreviewPresetResponse], error) {
	return c.client.PreviewPreset(ctx, req)
}
func (c *connAgent) GetConfig(ctx context.Context, req *connect.Request[agentv1.GetConfigRequest]) (*connect.Response[agentv1.GetConfigResponse], error) {
	return c.client.GetConfig(ctx, req)
}
func (c *connAgent) SetConfig(ctx context.Context, req *connect.Request[agentv1.SetConfigRequest]) (*connect.Response[agentv1.SetConfigResponse], error) {
	return c.client.SetConfig(ctx, req)
}
func (c *connAgent) ListTools(ctx context.Context, req *connect.Request[agentv1.ListToolsRequest]) (*connect.Response[agentv1.ListToolsResponse], error) {
	return c.client.ListTools(ctx, req)
}
func (c *connAgent) GetToolConfig(ctx context.Context, req *connect.Request[agentv1.GetToolConfigRequest]) (*connect.Response[agentv1.GetToolConfigResponse], error) {
	return c.client.GetToolConfig(ctx, req)
}
func (c *connAgent) SetToolConfig(ctx context.Context, req *connect.Request[agentv1.SetToolConfigRequest]) (*connect.Response[agentv1.SetToolConfigResponse], error) {
	return c.client.SetToolConfig(ctx, req)
}
func (c *connAgent) SetExtensionConfig(ctx context.Context, req *connect.Request[agentv1.SetExtensionConfigRequest]) (*connect.Response[agentv1.SetExtensionConfigResponse], error) {
	return c.client.SetExtensionConfig(ctx, req)
}
func (c *connAgent) UploadFile(ctx context.Context, req *connect.Request[agentv1.UploadFileRequest]) (*connect.Response[agentv1.UploadFileResponse], error) {
	return c.client.UploadFile(ctx, req)
}
func (c *connAgent) IngestFile(ctx context.Context, req *connect.Request[agentv1.IngestFileRequest]) (*connect.Response[agentv1.IngestFileResponse], error) {
	return c.client.IngestFile(ctx, req)
}
func (c *connAgent) GetFile(ctx context.Context, req *connect.Request[agentv1.GetFileRequest]) (*connect.Response[agentv1.GetFileResponse], error) {
	return c.client.GetFile(ctx, req)
}
func (c *connAgent) GetFileMeta(ctx context.Context, req *connect.Request[agentv1.GetFileMetaRequest]) (*connect.Response[agentv1.GetFileMetaResponse], error) {
	return c.client.GetFileMeta(ctx, req)
}
func (c *connAgent) ListWorksheets(ctx context.Context, req *connect.Request[agentv1.ListWorksheetsRequest]) (*connect.Response[agentv1.ListWorksheetsResponse], error) {
	return c.client.ListWorksheets(ctx, req)
}
func (c *connAgent) DecideWorksheet(ctx context.Context, req *connect.Request[agentv1.DecideWorksheetRequest]) (*connect.Response[agentv1.DecideWorksheetResponse], error) {
	return c.client.DecideWorksheet(ctx, req)
}
func (c *connAgent) GetZergxConfig(ctx context.Context, req *connect.Request[agentv1.GetZergxConfigRequest]) (*connect.Response[agentv1.GetZergxConfigResponse], error) {
	return c.client.GetZergxConfig(ctx, req)
}
