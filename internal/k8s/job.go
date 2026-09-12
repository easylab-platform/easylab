package k8s

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"connectrpc.com/connect"
	workerv1 "github.com/easylab-platform/easylab-proto/worker/v1"
	"github.com/easylab-platform/easylab-proto/worker/v1/workerv1connect"
	"github.com/easylab-platform/easylab/internal/connectauth"
)

// workerHostDir returns the node hostPath directory that holds the worker
// binary (published by easylab at /data/worker).
func workerHostDir() string {
	root := os.Getenv("EASYLAB_HOST_DATA_DIR")
	if root == "" {
		return ""
	}
	return filepath.Join(root, "worker")
}

// JobCommand is one command a job runs, in order, through the worker API.
type JobCommand struct {
	Name    string
	Run     string
	Workdir string
	Env     map[string]string
}

// JobSpec is one ephemeral worker job: launch a worker-backed container (plus
// any caller-supplied sidecars), synchronize a workspace tree, run commands
// through the worker API, stream logs, then destroy it. This is the single
// primitive behind sandboxes, CI steps and image builds.
type JobSpec struct {
	Name      string
	Image     string // worker image (linux derived, VM image, or build toolchain)
	Workspace string // default /workspace
	// Env are extra container env (the caller adds WORKER_TOKEN via Token).
	Env map[string]string
	// WorkerPort is the worker's listen port (default 48080).
	WorkerPort int32
	// Tarball, when non-nil, is unpacked into the workspace before commands.
	Tarball []byte
	// Commands run sequentially; a non-zero exit fails the job.
	Commands []JobCommand
	// Timeout bounds the whole job (0 => 30m).
	Timeout time.Duration

	Profile *RuntimeProfile

	// WorkerBinHostDir injects the worker binary from a hostPath directory
	// (CI jobs that use a toolchain base image instead of a derived image).
	WorkerBinHostDir string
}

// JobResult is a completed job's outcome.
type JobResult struct {
	ExitCode int
	Output   string
}

// RunJob launches the worker job, executes every command, and always cleans up
// the pod. onLog receives streamed output lines.
func (c *Client) RunJob(ctx context.Context, spec JobSpec, token string, onLog func(string)) (JobResult, error) {
	workspace := spec.Workspace
	if workspace == "" {
		workspace = "/workspace"
	}
	port := spec.WorkerPort
	if port == 0 {
		port = 48080
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	jctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if token == "" {
		t, err := c.randomToken()
		if err != nil {
			return JobResult{}, err
		}
		token = t
	}

	env := map[string]string{}
	for k, v := range spec.Env {
		env[k] = v
	}
	env["WORKER_TOKEN"] = token

	profile := spec.Profile
	if profile == nil {
		profile = &RuntimeProfile{}
	}
	if spec.Image == "" {
		return JobResult{}, fmt.Errorf("job %s: image required", spec.Name)
	}
	if _, err := c.LaunchSandbox(jctx, SandboxSpec{
		Name: spec.Name, Image: spec.Image, Workspace: workspace, Env: env,
		WorkerPort: port, Profile: profile,
		WorkerBinHostDir: spec.WorkerBinHostDir,
		NeedsTun:         profile.NeedsTun,
		DeviceLimits:     profile.DeviceLimits,
		NodeSelector:     profile.NodeSelector,
		NoService:        true,
	}); err != nil {
		return JobResult{}, err
	}
	defer func() {
		_ = c.DeleteSandbox(context.Background(), spec.Name)
	}()

	podIP, err := c.waitPodIP(jctx, spec.Name, port, profile.ReadyTimeoutDuration())
	if err != nil {
		return JobResult{}, err
	}

	w := newWorkerClient(podIP, port, token)
	defer w.close()

	if spec.Tarball != nil {
		if _, err := w.syncFolder(jctx, spec.Tarball); err != nil {
			return JobResult{}, fmt.Errorf("sync workspace: %w", err)
		}
	}

	var all string
	for _, cmd := range spec.Commands {
		if onLog != nil {
			label := cmd.Name
			if label == "" {
				label = cmd.Run
			}
			onLog("$ " + label)
		}
		out, code, err := w.run(jctx, cmd, onLog)
		all += out
		if err != nil {
			return JobResult{ExitCode: code, Output: all}, err
		}
		if code != 0 {
			return JobResult{ExitCode: code, Output: all}, fmt.Errorf("command exited %d: %s", code, cmd.Run)
		}
	}
	return JobResult{ExitCode: 0, Output: all}, nil
}

// waitPodIP polls until the pod is Running with a reachable worker port.
func (c *Client) waitPodIP(ctx context.Context, name string, port int32, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastIP string
	for time.Now().Before(deadline) {
		st, err := c.Status(ctx, name)
		if err == nil && st.Phase == "Running" && st.PodIP != "" {
			lastIP = st.PodIP
			if dialHealth(ctx, st.PodIP, port) == nil {
				return st.PodIP, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	if lastIP != "" {
		return "", fmt.Errorf("worker on %s:%d not healthy in %s", lastIP, port, timeout)
	}
	return "", fmt.Errorf("job %s not ready in %s", name, timeout)
}

// workerClient is a minimal bearer-authenticated worker.v1 client.
type workerClient struct {
	c     workerv1connect.WorkerServiceClient
	close func()
}

var jobHTTPTransport = &http.Transport{
	MaxIdleConns: 8, IdleConnTimeout: 90 * time.Second,
}

func newWorkerClient(ip string, port int32, token string) *workerClient {
	base := fmt.Sprintf("http://%s:%d", ip, port)
	opts := []connect.ClientOption{}
	if token != "" {
		opts = append(opts, connect.WithInterceptors(connectauth.Bearer(token)))
	}
	hc := &http.Client{Transport: jobHTTPTransport}
	return &workerClient{
		c:     workerv1connect.NewWorkerServiceClient(hc, base, opts...),
		close: func() {},
	}
}

func (w *workerClient) syncFolder(ctx context.Context, tarball []byte) (*workerv1.SyncFolderResponse, error) {
	res, err := w.c.SyncFolder(ctx, connect.NewRequest(&workerv1.SyncFolderRequest{
		Tarball: tarball, Dest: ".", Clean: true,
	}))
	if err != nil {
		return nil, err
	}
	return res.Msg, nil
}

// run executes one command and streams its output; returns (output, exitCode).
func (w *workerClient) run(ctx context.Context, cmd JobCommand, onLog func(string)) (string, int, error) {
	exec, err := w.c.Execute(ctx, connect.NewRequest(&workerv1.ExecuteRequest{
		Command: cmd.Run, Workdir: cmd.Workdir, Env: cmd.Env,
	}))
	if err != nil {
		return "", -1, err
	}
	jobID := exec.Msg.GetJobId()

	stream, err := w.c.WatchJob(ctx, connect.NewRequest(&workerv1.WatchJobRequest{JobId: jobID}))
	if err != nil {
		return "", -1, err
	}
	var out string
	for stream.Receive() {
		msg := stream.Msg()
		if o := msg.GetOutput(); o != "" {
			out += o
			if onLog != nil {
				for _, line := range splitLines(o) {
					onLog(line)
				}
			}
		}
		if d := msg.GetDone(); d != nil {
			if onLog != nil && d.GetStdout() != "" {
				for _, line := range splitLines(d.GetStdout()) {
					onLog(line)
				}
			}
			return out, int(d.GetExitCode()), nil
		}
	}
	if err := stream.Err(); err != nil {
		return out, -1, err
	}
	return out, -1, fmt.Errorf("job %s stream ended without a Done event", jobID)
}
