package opsext

import (
	"context"

	"connectrpc.com/connect"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	workerv1 "github.com/easylab-platform/easylab-proto/worker/v1"
	easylabclient "github.com/easylab-platform/easylab/internal/easylabclient"
)

// sandboxConn is the Connect-based replacement for the legacy WebSocket
// worker channel: every worker.v1 call is routed through the easylab gateway
// SandboxService (single entry), keyed by container name. No WS, no SSE.
//
// The generated SandboxService client is owned by the server; this type only
// maps the worker.v1 shapes onto it.
type sandboxConn struct {
	svc *easylabclient.Services
}

// newSandboxConn builds the Connect sandbox proxy.
func (s *server) sandboxConn() *sandboxConn { return &sandboxConn{svc: s.sdk} }

// Execute runs a command, returning the job id.
func (c *sandboxConn) Execute(ctx context.Context, cid, command string) (string, error) {
	res, err := c.svc.Sandbox.Execute(ctx, connect.NewRequest(&easylabv1.ExecuteRequest{
		Sandbox: cid, Req: &workerv1.ExecuteRequest{Command: command},
	}))
	if err != nil {
		return "", err
	}
	return res.Msg.GetJobId(), nil
}

// Jobs lists the jobs in the sandbox.
func (c *sandboxConn) Jobs(ctx context.Context, cid string) ([]map[string]interface{}, error) {
	res, err := c.svc.Sandbox.ListJobs(ctx, connect.NewRequest(&easylabv1.ListJobsRequest{Sandbox: cid}))
	if err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, len(res.Msg.GetJobs()))
	for _, j := range res.Msg.GetJobs() {
		out = append(out, map[string]interface{}{
			"id": j.Id, "command": j.Command, "state": j.State,
			"exit_code": j.ExitCode, "started_at": j.StartedAt, "finished_at": j.FinishedAt,
		})
	}
	return out, nil
}

// JobOutput paginates history. start may be negative.
func (c *sandboxConn) JobOutput(ctx context.Context, cid string, req *workerv1.JobOutputRequest) (*workerv1.JobOutputResponse, error) {
	res, err := c.svc.Sandbox.JobOutput(ctx, connect.NewRequest(&easylabv1.JobOutputRequest{Sandbox: cid, Req: req}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// JobWait blocks until a job completes or times out (worker caps at 60s).
func (c *sandboxConn) JobWait(ctx context.Context, cid string, req *workerv1.JobWaitRequest) (*workerv1.JobWaitResponse, error) {
	res, err := c.svc.Sandbox.JobWait(ctx, connect.NewRequest(&easylabv1.JobWaitRequest{Sandbox: cid, Req: req}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// JobStdin writes to a job's stdin (closeAfter ends the pipe).
func (c *sandboxConn) JobStdin(ctx context.Context, cid string, req *workerv1.JobStdinRequest) error {
	_, err := c.svc.Sandbox.JobStdin(ctx, connect.NewRequest(&easylabv1.JobStdinRequest{Sandbox: cid, Req: req}))
	return err
}

// JobKill stops a job (process-tree kill).
func (c *sandboxConn) JobKill(ctx context.Context, cid string, req *workerv1.JobKillRequest) error {
	_, err := c.svc.Sandbox.JobKill(ctx, connect.NewRequest(&easylabv1.JobKillRequest{Sandbox: cid, Req: req}))
	return err
}

// FileRead reads a sandbox file (bytes).
func (c *sandboxConn) FileRead(ctx context.Context, cid, path string) ([]byte, error) {
	res, err := c.svc.Sandbox.FileRead(ctx, connect.NewRequest(&easylabv1.FileReadRequest{
		Sandbox: cid, Req: &workerv1.FileReadRequest{Path: path},
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetContent(), nil
}

// FileWrite writes a sandbox file (bytes).
func (c *sandboxConn) FileWrite(ctx context.Context, cid, path string, data []byte) error {
	_, err := c.svc.Sandbox.FileWrite(ctx, connect.NewRequest(&easylabv1.FileWriteRequest{
		Sandbox: cid, Req: &workerv1.FileWriteRequest{Path: path, Content: data},
	}))
	return err
}

// FileList lists a sandbox path (dir) or stats a single file.
func (c *sandboxConn) FileList(ctx context.Context, cid, path string) (bool, []*workerv1.FileEntry, error) {
	res, err := c.svc.Sandbox.FileList(ctx, connect.NewRequest(&easylabv1.FileListRequest{
		Sandbox: cid, Req: &workerv1.FileListRequest{Path: path},
	}))
	if err != nil {
		return false, nil, err
	}
	return res.Msg.GetIsDir(), res.Msg.GetFiles(), nil
}
