package opsext

import (
	"context"

	workerv1 "github.com/easylab-platform/easylab-proto/worker/v1"
	"github.com/easylab-platform/easylab-sdk-go"
)

// sandboxConn is the Connect-based replacement for the legacy WebSocket
// worker channel: every worker.v1 call is routed through the easylab gateway
// SandboxService (single entry), keyed by container name. No WS, no SSE.
type sandboxConn struct {
	sdk *easylabsdk.Client
	// container key (labelKey(session)) → it is the same as the easylab
	// sandbox name; easylab resolves it to the worker at publish time.
}

// newSandboxConn builds the Connect sandbox proxy.
func (s *server) sandboxConn() *sandboxConn { return &sandboxConn{sdk: s.sdk} }

// Execute runs a command, returning the job id.
func (c *sandboxConn) Execute(ctx context.Context, cid, command string) (string, error) {
	res, err := c.sdk.Sandbox().Execute(ctx, cid, &workerv1.ExecuteRequest{Command: command})
	if err != nil {
		return "", err
	}
	return res.Msg.JobId, nil
}

// Jobs lists the jobs in the sandbox.
func (c *sandboxConn) Jobs(ctx context.Context, cid string) ([]map[string]interface{}, error) {
	res, err := c.sdk.Sandbox().ListJobs(ctx, cid)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, len(res.Msg.Jobs))
	for _, j := range res.Msg.Jobs {
		out = append(out, map[string]interface{}{
			"id": j.Id, "command": j.Command, "state": j.State,
			"exit_code": j.ExitCode, "started_at": j.StartedAt, "finished_at": j.FinishedAt,
		})
	}
	return out, nil
}

// JobOutput paginates history. start may be negative.
func (c *sandboxConn) JobOutput(ctx context.Context, cid string, req *workerv1.JobOutputRequest) (*workerv1.JobOutputResponse, error) {
	res, err := c.sdk.Sandbox().JobOutput(ctx, cid, req)
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// JobWait blocks until a job completes or times out (worker caps at 60s).
func (c *sandboxConn) JobWait(ctx context.Context, cid string, req *workerv1.JobWaitRequest) (*workerv1.JobWaitResponse, error) {
	res, err := c.sdk.Sandbox().JobWait(ctx, cid, req)
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// JobStdin writes to a job's stdin (closeAfter ends the pipe).
func (c *sandboxConn) JobStdin(ctx context.Context, cid string, req *workerv1.JobStdinRequest) error {
	_, err := c.sdk.Sandbox().JobStdin(ctx, cid, req)
	return err
}

// JobKill stops a job (process-tree kill).
func (c *sandboxConn) JobKill(ctx context.Context, cid string, req *workerv1.JobKillRequest) error {
	_, err := c.sdk.Sandbox().JobKill(ctx, cid, req)
	return err
}

// FileRead reads a sandbox file (bytes).
func (c *sandboxConn) FileRead(ctx context.Context, cid, path string) ([]byte, error) {
	res, err := c.sdk.Sandbox().FileRead(ctx, cid, &workerv1.FileReadRequest{Path: path})
	if err != nil {
		return nil, err
	}
	return res.Msg.Content, nil
}

// FileWrite writes a sandbox file (bytes).
func (c *sandboxConn) FileWrite(ctx context.Context, cid, path string, data []byte) error {
	_, err := c.sdk.Sandbox().FileWrite(ctx, cid, &workerv1.FileWriteRequest{Path: path, Content: data})
	return err
}

// FileList lists a sandbox path (dir) or stats a single file.
func (c *sandboxConn) FileList(ctx context.Context, cid, path string) (bool, []*workerv1.FileEntry, error) {
	res, err := c.sdk.Sandbox().FileList(ctx, cid, &workerv1.FileListRequest{Path: path})
	if err != nil {
		return false, nil, err
	}
	return res.Msg.IsDir, res.Msg.Files, nil
}
