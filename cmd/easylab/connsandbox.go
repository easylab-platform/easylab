package main

import (
	"context"
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
	"github.com/easylab-platform/easylab/internal/ops"
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

// workerAddr resolves the sandbox worker's loopback address (published
// auto-port). The podman flavor has no routable container IPs — loopback
// publishing is the reachability model.
func (c *connSandbox) workerAddr(ctx context.Context, sandbox string) (string, error) {
	pr := c.s.sandboxRunner()
	if pr == nil {
		return "", fmt.Errorf("services backend unavailable")
	}
	addr, err := pr.WorkerAddr(ctx, sandbox, sandboxWorkerPort)
	if err != nil {
		return "", err
	}
	return addr, nil
}

// wc resolves the sandbox's worker client (127.0.0.1:<published>).
func (c *connSandbox) wc(ctx context.Context, sandbox string) (workerv1connect.WorkerServiceClient, error) {
	addr, err := c.workerAddr(ctx, sandbox)
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("sandbox %q: %v", sandbox, err))
	}
	return workerv1connect.NewWorkerServiceClient(
		&http.Client{Transport: workerTransport, Timeout: 65 * time.Second},
		"http://"+addr,
	), nil
}

// ---- lifecycle ----

func (c *connSandbox) EnsureSandboxImage(ctx context.Context, req *connect.Request[easylabv1.EnsureSandboxImageRequest]) (*connect.Response[easylabv1.EnsureSandboxImageResponse], error) {
	if req.Msg.BaseImage == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("base_image required"))
	}
	pr := c.s.sandboxRunner()
	if pr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("services backend unavailable"))
	}
	tag, built, err := pr.EnsureSandboxImage(ctx, req.Msg.BaseImage, "/workspace")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.EnsureSandboxImageResponse{DerivedImage: tag, Built: built}), nil
}

// LaunchSandbox: ensure derived image -> launch (worker PID1, restart=always)
// -> wait healthz -> optional Sync (org != "") -> registry upsert.
func (c *connSandbox) LaunchSandbox(ctx context.Context, req *connect.Request[easylabv1.LaunchSandboxRequest]) (*connect.Response[easylabv1.LaunchSandboxResponse], error) {
	m := req.Msg
	if m.Name == "" || m.BaseImage == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name and base_image required"))
	}
	pr := c.s.sandboxRunner()
	if pr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("services backend unavailable"))
	}
	workspace := m.Workspace
	if workspace == "" {
		workspace = "/workspace"
	}

	tag, _, err := pr.EnsureSandboxImage(ctx, m.BaseImage, workspace)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("ensure image: %w", err))
	}

	env := []string{"WORKER_WORKSPACE=" + workspace}
	for k, v := range m.Env {
		env = append(env, k+"="+v)
	}
	labels := map[string]string{
		"easylab/sandbox.image": m.BaseImage,
		"easylab/derived.image": tag,
		"easylab/sandbox":       "1",
	}
	if m.Org != "" {
		labels["easylab/org"], labels["easylab/repo"], labels["easylab/branch"] = m.Org, m.Repo, m.Branch
	}
	_, err = pr.Launch(ctx, ops.ServiceRequest{
		Name:        m.Name,
		Image:       tag,
		Ports:       map[int]int{sandboxWorkerPort: -1}, // -1 = auto loopback publish
		Env:         env,
		Restart:     "always",
		CPUs:        m.Cpus,
		MemoryBytes: m.MemoryBytes,
		Labels:      labels,
	}, nil)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("launch: %w", err))
	}

	// Wait for the worker to answer healthz on its published loopback port.
	var healthy bool
	for i := 0; i < 40; i++ {
		addr, _ := pr.WorkerAddr(ctx, m.Name, sandboxWorkerPort)
		if addr != "" {
			if err := probeHealth(ctx, addr); err == nil {
				healthy = true
				break
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if !healthy {
		_ = pr.Delete(ctx, m.Name) // roll back a half-started sandbox
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("worker did not become healthy on %s", m.Name))
	}

	// Register (standalone when org == "") and sync associated workspaces.
	if err := c.s.sbx.Upsert(sbxreg.Sandbox{
		Name: m.Name, Org: m.Org, Repo: m.Repo, Branch: m.Branch,
		BaseImage: m.BaseImage, DerivedImage: tag, Workspace: workspace,
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

func probeHealth(ctx context.Context, addr string) error {
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(cctx, http.MethodGet, "http://"+addr+"/healthz", nil)
	resp, err := workerTransport.RoundTrip(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz %d", resp.StatusCode)
	}
	return nil
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
	pr := c.s.sandboxRunner()
	if pr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("services backend unavailable"))
	}
	if err := pr.Delete(ctx, req.Msg.Name); err != nil && !strings.Contains(err.Error(), "No such") {
		return connect.NewResponse(&easylabv1.DeleteSandboxResponse{Error: err.Error()}), nil
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
	if addr, err := c.workerAddr(ctx, name); err == nil && addr != "" {
		info.PodIp = addr
		info.Phase = "Running"
		wctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if w, err := c.wc(wctx, name); err == nil {
			if inf, err := w.Info(wctx, connect.NewRequest(&workerv1.InfoRequest{})); err == nil {
				info.BootId = inf.Msg.BootId
			} else {
				info.Error = "worker: " + err.Error()
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
	} else {
		info.Phase = "stopped"
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
		Workspace: row.Workspace, SyncedRev: row.SyncedRev, SyncedBootId: row.SyncedBootID,
	}
}
