package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	easylabv1connect "github.com/easylab-platform/easylab-proto/easylab/v1/easylabv1connect"
	workerv1 "github.com/easylab-platform/easylab-proto/worker/v1"
	"github.com/easylab-platform/easylab-proto/worker/v1/workerv1connect"
	"github.com/easylab-platform/easylab/internal/connectauth"
	"github.com/easylab-platform/easylab/internal/k8s"
	"github.com/easylab-platform/easylab/internal/sbxreg"
)

// sandboxWorkerPort is the fixed easyworker port inside sandbox containers.
const sandboxWorkerPort = 48080

// connSandbox implements easylabv1connect.SandboxServiceHandler: lifecycle
// (derived image + launch + sync + registry table) plus worker.v1
// passthroughs for the UI (jobs observability, console, files). The frontend
// only ever talks to easylab — same single-entry model as the agent proxy.
type connSandbox struct {
	s *server
}

var _ easylabv1connect.SandboxServiceHandler = (*connSandbox)(nil)

// ---- worker dialing ----

var workerTransport = &http.Transport{
	MaxIdleConns: 32, MaxIdleConnsPerHost: 4,
	DialContext:     (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	IdleConnTimeout: 90 * time.Second,
}

// workerAddr resolves the sandbox worker's base address. External sandboxes
// carry an explicit Addr; managed sandboxes use the k8s Service DNS
// (<name>.<ns>.svc:48080).
func (c *connSandbox) workerAddr(ctx context.Context, sandbox string) (string, error) {
	if row, ok, err := c.s.sbx.Get(sandbox); err == nil && ok {
		if row.Mode == "external" && row.Addr != "" {
			return row.Addr, nil
		}
	}
	if c.s.k8s == nil {
		return "", fmt.Errorf("k8s backend unavailable")
	}
	return "http://" + c.s.k8s.ServiceDNS(sandbox) + ":" + fmt.Sprint(sandboxWorkerPort), nil
}

func (c *connSandbox) workerBearer(sandbox string) string {
	if row, ok, err := c.s.sbx.Get(sandbox); err == nil && ok {
		return row.Token
	}
	return ""
}

// wc resolves the sandbox's worker client, bearer-authenticated with the
// sandbox's recorded token.
func (c *connSandbox) wc(ctx context.Context, sandbox string) (workerv1connect.WorkerServiceClient, error) {
	base, err := c.workerAddr(ctx, sandbox)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("sandbox %q: %v", sandbox, err))
	}
	opts := []connect.ClientOption{}
	if tok := c.workerBearer(sandbox); tok != "" {
		opts = append(opts, connect.WithInterceptors(connectauth.Bearer(tok)))
	}
	return workerv1connect.NewWorkerServiceClient(
		&http.Client{Transport: workerTransport, Timeout: 65 * time.Second},
		base, opts...,
	), nil
}

// ---- lifecycle ----

func (c *connSandbox) EnsureSandboxImage(ctx context.Context, req *connect.Request[easylabv1.EnsureSandboxImageRequest]) (*connect.Response[easylabv1.EnsureSandboxImageResponse], error) {
	if c.s.k8s == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("k8s backend unavailable"))
	}
	profile, err := c.s.k8s.ResolveRuntime(req.Msg.Runtime)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if profile.Derived && req.Msg.BaseImage == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("base_image required"))
	}
	tag, built, err := c.s.ensureSandboxImage(ctx, profile, req.Msg.BaseImage, "/workspace")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.EnsureSandboxImageResponse{DerivedImage: tag, Built: built}), nil
}

// LaunchSandbox: ensure derived image -> launch (worker PID1, restart=always)
// -> wait healthz -> optional Sync (org != "") -> registry upsert.
func (c *connSandbox) LaunchSandbox(ctx context.Context, req *connect.Request[easylabv1.LaunchSandboxRequest]) (*connect.Response[easylabv1.LaunchSandboxResponse], error) {
	m := req.Msg
	if m.Name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name required"))
	}
	if c.s.k8s == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("k8s backend unavailable"))
	}
	profile, err := c.s.k8s.ResolveRuntime(m.Runtime)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	// base_image drives the derived (linux) build; VM runtimes carry their own
	// image in the profile.
	if profile.Derived && m.BaseImage == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("base_image required"))
	}
	workspace := m.Workspace
	if workspace == "" {
		workspace = "/workspace"
	}
	tag, _, err := c.s.ensureSandboxImage(ctx, profile, m.BaseImage, workspace)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("ensure image: %w", err))
	}
	token, err := newSandboxToken()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("mint sandbox token: %w", err))
	}
	// Profile defaults first (VM parameters), then request env, then the token
	// (always authoritative).
	env := map[string]string{}
	for k, v := range profile.Env {
		env[k] = v
	}
	for k, v := range m.Env {
		env[k] = v
	}
	env["WORKER_TOKEN"] = token
	if _, err := c.s.k8s.LaunchSandbox(ctx, k8s.SandboxSpec{
		Name: m.Name, Image: tag, Workspace: workspace, Env: env,
		NeedsTun: profile.NeedsTun, DeviceLimits: profile.DeviceLimits,
		NodeSelector: profile.NodeSelector, WorkerPort: profile.Port(),
		Profile: &profile,
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("launch: %w", err))
	}
	if err := c.s.k8s.WaitSandboxReady(ctx, m.Name, profile.Port(), profile.ReadyTimeoutDuration()); err != nil {
		_ = c.s.k8s.DeleteSandbox(ctx, m.Name)
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("worker did not become healthy on %s: %w", m.Name, err))
	}
	if err := c.s.sbx.Upsert(sbxreg.Sandbox{
		Name: m.Name, Org: m.Org, Repo: m.Repo, Branch: m.Branch,
		BaseImage: m.BaseImage, DerivedImage: tag, Workspace: workspace,
		Runtime: profile.Name, Token: token, Mode: "managed",
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("registry: %w", err))
	}
	if m.Org != "" && m.Repo != "" && m.Branch != "" {
		if err := c.s.syncSandboxWorkspace(ctx, m.Name, m.Org, m.Repo, m.Branch); err != nil {
			return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("sync: %w", err))
		}
	}
	info, err := c.sandboxInfo(ctx, m.Name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.LaunchSandboxResponse{Sandbox: info}), nil
}

// SyncWorkspace pushes the workspace tree into the sandbox and records
// rev + boot id (rev coherence). Used by ext-ops' ensureSynced path.
func (c *connSandbox) SyncWorkspace(ctx context.Context, req *connect.Request[easylabv1.SyncWorkspaceRequest]) (*connect.Response[easylabv1.SyncWorkspaceResponse], error) {
	m := req.Msg
	if m.Sandbox == "" || m.Org == "" || m.Repo == "" || m.Rev == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("sandbox/org/repo/rev required"))
	}
	if err := c.s.syncSandboxWorkspace(ctx, m.Sandbox, m.Org, m.Repo, m.Branch); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	row, _, _ := c.s.sbx.Get(m.Sandbox)
	return connect.NewResponse(&easylabv1.SyncWorkspaceResponse{SyncedRev: row.SyncedRev}), nil
}

func (c *connSandbox) DeleteSandbox(ctx context.Context, req *connect.Request[easylabv1.DeleteSandboxRequest]) (*connect.Response[easylabv1.DeleteSandboxResponse], error) {
	if c.s.k8s != nil {
		if err := c.s.k8s.DeleteSandbox(ctx, req.Msg.Name); err != nil {
			return connect.NewResponse(&easylabv1.DeleteSandboxResponse{Error: err.Error()}), nil
		}
	}
	_ = c.s.sbx.Delete(req.Msg.Name)
	return connect.NewResponse(&easylabv1.DeleteSandboxResponse{Ok: true}), nil
}

// ---- observability ----

// sandboxCacheTTL bounds the ListSandboxes fanout.
const sandboxCacheTTL = 5 * time.Second

var (
	sbxCacheMu   sync.Mutex
	sbxCacheAt   time.Time
	sbxCacheList []*easylabv1.SandboxInfo
)

func (c *connSandbox) ListSandboxes(ctx context.Context, req *connect.Request[easylabv1.ListSandboxesRequest]) (*connect.Response[easylabv1.ListSandboxesResponse], error) {
	sbxCacheMu.Lock()
	if time.Since(sbxCacheAt) < sandboxCacheTTL && sbxCacheList != nil {
		out := sbxCacheList
		sbxCacheMu.Unlock()
		return connect.NewResponse(&easylabv1.ListSandboxesResponse{Sandboxes: out}), nil
	}
	sbxCacheMu.Unlock()

	rows, err := c.s.sbx.List()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	var wg sync.WaitGroup
	infos := make([]*easylabv1.SandboxInfo, len(rows))
	for i, row := range rows {
		infos[i] = rowToInfo(row)
		wg.Add(1)
		go func(i int, name string) {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, 1200*time.Millisecond)
			defer cancel()
			if info, err := c.sandboxInfo(cctx, name); err == nil {
				infos[i] = info
			} else {
				infos[i].Error = err.Error()
			}
		}(i, row.Name)
	}
	wg.Wait()

	sbxCacheMu.Lock()
	sbxCacheAt, sbxCacheList = time.Now(), infos
	sbxCacheMu.Unlock()
	return connect.NewResponse(&easylabv1.ListSandboxesResponse{Sandboxes: infos}), nil
}

func (c *connSandbox) GetSandbox(ctx context.Context, req *connect.Request[easylabv1.GetSandboxRequest]) (*connect.Response[easylabv1.GetSandboxResponse], error) {
	info, err := c.sandboxInfo(ctx, req.Msg.Name)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&easylabv1.GetSandboxResponse{Sandbox: info}), nil
}

// sandboxInfo merges the registry row, live podman status and worker stats.
func (c *connSandbox) sandboxInfo(ctx context.Context, name string) (*easylabv1.SandboxInfo, error) {
	row, ok, err := c.s.sbx.Get(name)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("sandbox %q not registered", name)
	}
	info := rowToInfo(row)
	if c.s.k8s != nil {
		if st, serr := c.s.k8s.Status(ctx, name); serr == nil {
			info.PodIp = st.PodIP
			info.Phase = st.Phase
			if st.Ready {
				info.Phase = "Running"
			}
		} else {
			info.Phase = "stopped"
		}
	}
	if addr, err := c.workerAddr(ctx, name); err == nil && addr != "" {
		if w, err := c.wc(ctx, name); err == nil {
			wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			defer cancel()
			if inf, err := w.Info(wctx, connect.NewRequest(&workerv1.InfoRequest{})); err == nil {
				info.BootId = inf.Msg.BootId
			}
			if jobs, err := w.ListJobs(wctx, connect.NewRequest(&workerv1.ListJobsRequest{})); err == nil {
				info.TotalJobs = int32(len(jobs.Msg.Jobs))
				for _, j := range jobs.Msg.Jobs {
					if j.State == "running" {
						info.RunningJobs++
					}
				}
			}
		}
	}
	return info, nil
}

// ---- worker.v1 passthroughs ----

func (c *connSandbox) Execute(ctx context.Context, req *connect.Request[easylabv1.ExecuteRequest]) (*connect.Response[workerv1.ExecuteResponse], error) {
	w, err := c.wc(ctx, req.Msg.Sandbox)
	if err != nil {
		return nil, err
	}
	return w.Execute(ctx, connect.NewRequest(req.Msg.Req))
}

func (c *connSandbox) ListJobs(ctx context.Context, req *connect.Request[easylabv1.ListJobsRequest]) (*connect.Response[workerv1.ListJobsResponse], error) {
	w, err := c.wc(ctx, req.Msg.Sandbox)
	if err != nil {
		return nil, err
	}
	res, err := w.ListJobs(ctx, connect.NewRequest(&workerv1.ListJobsRequest{}))
	if err != nil {
		return nil, err
	}
	if req.Msg.Limit > 0 && int(req.Msg.Limit) < len(res.Msg.Jobs) {
		res.Msg.Jobs = res.Msg.Jobs[:req.Msg.Limit]
	}
	return res, nil
}

func (c *connSandbox) JobOutput(ctx context.Context, req *connect.Request[easylabv1.JobOutputRequest]) (*connect.Response[workerv1.JobOutputResponse], error) {
	w, err := c.wc(ctx, req.Msg.Sandbox)
	if err != nil {
		return nil, err
	}
	return w.JobOutput(ctx, connect.NewRequest(req.Msg.Req))
}

// WatchJob streams worker output through the gateway: history replay, live
// updates and the terminal Done event (same shape as the agent proxy).
func (c *connSandbox) WatchJob(ctx context.Context, req *connect.Request[easylabv1.WatchJobRequest], srv *connect.ServerStream[workerv1.WatchJobResponse]) error {
	w, err := c.wc(ctx, req.Msg.Sandbox)
	if err != nil {
		return err
	}
	stream, err := w.WatchJob(ctx, connect.NewRequest(req.Msg.Req))
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

func (c *connSandbox) JobWait(ctx context.Context, req *connect.Request[easylabv1.JobWaitRequest]) (*connect.Response[workerv1.JobWaitResponse], error) {
	w, err := c.wc(ctx, req.Msg.Sandbox)
	if err != nil {
		return nil, err
	}
	return w.JobWait(ctx, connect.NewRequest(req.Msg.Req))
}

func (c *connSandbox) JobStdin(ctx context.Context, req *connect.Request[easylabv1.JobStdinRequest]) (*connect.Response[workerv1.JobStdinResponse], error) {
	w, err := c.wc(ctx, req.Msg.Sandbox)
	if err != nil {
		return nil, err
	}
	return w.JobStdin(ctx, connect.NewRequest(req.Msg.Req))
}

func (c *connSandbox) JobKill(ctx context.Context, req *connect.Request[easylabv1.JobKillRequest]) (*connect.Response[workerv1.JobKillResponse], error) {
	w, err := c.wc(ctx, req.Msg.Sandbox)
	if err != nil {
		return nil, err
	}
	return w.JobKill(ctx, connect.NewRequest(req.Msg.Req))
}

func (c *connSandbox) FileRead(ctx context.Context, req *connect.Request[easylabv1.FileReadRequest]) (*connect.Response[workerv1.FileReadResponse], error) {
	w, err := c.wc(ctx, req.Msg.Sandbox)
	if err != nil {
		return nil, err
	}
	return w.FileRead(ctx, connect.NewRequest(req.Msg.Req))
}

func (c *connSandbox) FileWrite(ctx context.Context, req *connect.Request[easylabv1.FileWriteRequest]) (*connect.Response[workerv1.FileWriteResponse], error) {
	w, err := c.wc(ctx, req.Msg.Sandbox)
	if err != nil {
		return nil, err
	}
	return w.FileWrite(ctx, connect.NewRequest(req.Msg.Req))
}

func (c *connSandbox) FileList(ctx context.Context, req *connect.Request[easylabv1.FileListRequest]) (*connect.Response[workerv1.FileListResponse], error) {
	w, err := c.wc(ctx, req.Msg.Sandbox)
	if err != nil {
		return nil, err
	}
	return w.FileList(ctx, connect.NewRequest(req.Msg.Req))
}

// rowToInfo renders a registry row as the API shape.
func rowToInfo(row sbxreg.Sandbox) *easylabv1.SandboxInfo {
	return &easylabv1.SandboxInfo{
		Name: row.Name, Org: row.Org, Repo: row.Repo, Branch: row.Branch,
		BaseImage: row.BaseImage, DerivedImage: row.DerivedImage,
		Workspace: row.Workspace, Runtime: row.Runtime,
		SyncedRev: row.SyncedRev, SyncedBootId: row.SyncedBootID,
	}
}

// newSandboxToken mints a per-sandbox bearer token (the worker installs it from
// WORKER_TOKEN and requires it on every WorkerService call).
func newSandboxToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// RegisterExternalSandbox adopts an externally-run worker: it either claims the
// worker with a one-time enrollment code (exclusive) or accepts a
// pre-provisioned token. The token + address are persisted so easylab's worker
// passthroughs keep working across restarts.
func (c *connSandbox) RegisterExternalSandbox(ctx context.Context, req *connect.Request[easylabv1.RegisterExternalSandboxRequest]) (*connect.Response[easylabv1.RegisterExternalSandboxResponse], error) {
	m := req.Msg
	if m.Name == "" || m.Addr == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name and addr required"))
	}
	if m.Code == "" && m.Token == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("code or token required"))
	}
	base := strings.TrimSuffix(m.Addr, "/")

	token := m.Token
	if token == "" {
		whc := &http.Client{Timeout: 15 * time.Second}
		wres, err := workerv1connect.NewWorkerEnrollClient(whc, base).Claim(ctx,
			connect.NewRequest(&workerv1.EnrollClaimRequest{Code: m.Code, OwnerId: m.Owner}))
		if err != nil {
			code := connect.CodeOf(err)
			if code == connect.CodeAlreadyExists {
				return nil, connect.NewError(connect.CodeAlreadyExists,
					fmt.Errorf("worker %s already claimed by another caller", m.Name))
			}
			return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("enroll %s: %w", m.Name, err))
		}
		token = wres.Msg.GetToken()
	}
	if token == "" {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("worker %s returned no token", m.Name))
	}

	// Verify the token actually works before persisting.
	vc := workerv1connect.NewWorkerServiceClient(&http.Client{Transport: workerTransport, Timeout: 10 * time.Second},
		base, connect.WithInterceptors(connectauth.Bearer(token)))
	info, err := vc.Info(ctx, connect.NewRequest(&workerv1.InfoRequest{}))
	if err != nil {
		return nil, connect.NewError(connect.CodeUnauthenticated, fmt.Errorf("verify %s: %w", m.Name, err))
	}

	if err := c.s.sbx.Upsert(sbxreg.Sandbox{
		Name: m.Name, Org: m.Org, Repo: m.Repo, Branch: m.Branch,
		Token: token, Mode: "external", Addr: base, OwnerID: m.Owner,
		SyncedBootID: info.Msg.GetBootId(),
	}); err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("registry: %w", err))
	}
	return connect.NewResponse(&easylabv1.RegisterExternalSandboxResponse{
		Ok: true, Token: token,
	}), nil
}

// ListExternalSandboxes returns the externally-registered workers (mode =
// external) with a live reachability probe. Managed sandboxes are excluded.
func (c *connSandbox) ListExternalSandboxes(ctx context.Context, req *connect.Request[easylabv1.ListExternalSandboxesRequest]) (*connect.Response[easylabv1.ListExternalSandboxesResponse], error) {
	rows, err := c.s.sbx.List()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*easylabv1.ExternalSandbox, 0)
	for _, row := range rows {
		if row.Mode != "external" {
			continue
		}
		es := &easylabv1.ExternalSandbox{
			Name: row.Name, Addr: row.Addr, Org: row.Org, Repo: row.Repo,
			Branch: row.Branch, Owner: row.OwnerID,
			SyncedRev: row.SyncedRev, SyncedBootId: row.SyncedBootID,
		}
		if row.Addr != "" && row.Token != "" {
			wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
			vc := workerv1connect.NewWorkerServiceClient(
				&http.Client{Transport: workerTransport, Timeout: 3 * time.Second},
				strings.TrimSuffix(row.Addr, "/"), connect.WithInterceptors(connectauth.Bearer(row.Token)))
			if _, err := vc.Info(wctx, connect.NewRequest(&workerv1.InfoRequest{})); err == nil {
				es.Reachable = true
			} else {
				es.Error = err.Error()
			}
			cancel()
		}
		out = append(out, es)
	}
	return connect.NewResponse(&easylabv1.ListExternalSandboxesResponse{Sandboxes: out}), nil
}

// ReleaseExternalSandbox revokes easylab's token on the worker and returns the
// worker to the claimable state with a fresh one-time code; easylab drops its
// registration. A released worker can be claimed by any caller with the code.
func (c *connSandbox) ReleaseExternalSandbox(ctx context.Context, req *connect.Request[easylabv1.ReleaseExternalSandboxRequest]) (*connect.Response[easylabv1.ReleaseExternalSandboxResponse], error) {
	name := req.Msg.Name
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name required"))
	}
	row, ok, err := c.s.sbx.Get(name)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if !ok {
		return connect.NewResponse(&easylabv1.ReleaseExternalSandboxResponse{Ok: false, Error: "unknown sandbox"}), nil
	}
	if row.Mode != "external" {
		return nil, connect.NewError(connect.CodeFailedPrecondition,
			fmt.Errorf("sandbox %s is managed (mode=%q); only external sandboxes can be released", name, row.Mode))
	}
	if row.Addr == "" || row.Token == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("sandbox %s has no addr/token", name))
	}
	base := strings.TrimSuffix(row.Addr, "/")
	rel, err := workerv1connect.NewWorkerEnrollClient(
		&http.Client{Timeout: 15 * time.Second}, base,
		connect.WithInterceptors(connectauth.Bearer(row.Token)),
	).Unrelease(ctx, connect.NewRequest(&workerv1.EnrollUnreleaseRequest{OwnerId: row.OwnerID}))
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("release worker %s: %w", name, err))
	}
	if err := c.s.sbx.Delete(name); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.ReleaseExternalSandboxResponse{Ok: true, Code: rel.Msg.GetCode()}), nil
}
