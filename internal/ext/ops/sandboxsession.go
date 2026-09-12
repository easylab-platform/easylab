package opsext

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
)

// ExecuteResult is the normalized outcome of a job launch (the tools layer
// reads the job id, then the terminal snapshot separately).
type ExecuteResult struct {
	JobID string
}

// JobDone is the terminal snapshot of a job (exit code + output tail).
type JobDone struct {
	ExitCode int32
	Stdout   string
	Stderr   string
	// Bg marks a job whose wait window elapsed while still running: it stays
	// registered in the sandbox and can be driven by job-id (stdin/kill/wait).
	Bg bool
}

// workerInfo resolves a sandbox's live state via easylab SandboxService.
func (s *server) workerInfo(ctx context.Context, key string) (ContainerInfo, error) {
	res, err := s.sdk.Sandbox.GetSandbox(ctx, connect.NewRequest(&easylabv1.GetSandboxRequest{Name: key}))
	if err != nil {
		return ContainerInfo{}, err
	}
	if res == nil || res.Msg == nil || res.Msg.Sandbox == nil {
		return ContainerInfo{}, fmt.Errorf("sandbox %q not registered", key)
	}
	info := res.Msg.Sandbox
	return ContainerInfo{
		ContainerID: info.Name,
		SessionName: info.Name,
		Status:      info.Phase,
		PodIP:       info.PodIp,
	}, nil
}

// ensureSynced is the rev-coherence call: easylab owns the idempotent
// skip-if-already-synced logic (compares synced_rev + live boot id against
// the branch head). ops-extension just names the target and the workspace.
func (s *server) ensureSynced(ctx context.Context, cid, session string, ws workspace) error {
	if _, err := s.sdk.Sandbox.SyncWorkspace(ctx, connect.NewRequest(&easylabv1.SyncWorkspaceRequest{
		Sandbox: cid, Org: ws.org, Repo: ws.repo, Rev: ws.rev, Branch: ws.branchOrDefault(),
	})); err != nil {
		// Treat "already synced" (not-found on branch) as success to keep the
		// hot path idempotent; easylab surfaces real errors otherwise.
		if strings.Contains(err.Error(), "not found") {
			return nil
		}
		return fmt.Errorf("sync: %w", err)
	}
	return nil
}

// ensureSandbox resolves the workspace and requires the session's worker
// sandbox to already exist. It does NOT create it. When present and needSync,
// the workspace branch head is synced (idempotent, easylab-owned).
func (s *server) ensureSandbox(ctx context.Context, args map[string]interface{}, sessionName string, needSync bool) (sandboxCtx, error) {
	ws, sid, err := s.resolveWorkspace(ctx, args, sessionName)
	if err != nil {
		return sandboxCtx{}, err
	}
	key := labelKey(sid)
	info, err := s.workerInfo(ctx, key)
	if err != nil {
		return sandboxCtx{}, fmt.Errorf("sandbox not created — call sandbox-create with an image first")
	}
	if !strings.EqualFold(info.Status, "running") {
		return sandboxCtx{}, fmt.Errorf("sandbox %s is %s (not running)", key, info.Status)
	}
	s.publishSandboxVars(ctx, sid, info)
	sc := sandboxCtx{session: sid, cid: info.ContainerID, ws: ws}
	if needSync {
		if err := s.ensureSynced(ctx, sc.cid, sc.session, ws); err != nil {
			return sandboxCtx{}, err
		}
	}
	return sc, nil
}

// launchWorkspaceSandbox launches a worker-backed sandbox via easylab
// SandboxService, associated with this session's workspace (idempotent).
func (s *server) launchWorkspaceSandbox(ctx context.Context, ws workspace, sid, baseImage, runtime string) (ContainerInfo, error) {
	key := labelKey(sid)
	res, err := s.sdk.Sandbox.LaunchSandbox(ctx, connect.NewRequest(&easylabv1.LaunchSandboxRequest{
		Name: key, BaseImage: baseImage, Runtime: runtime,
		Org: ws.org, Repo: ws.repo, Branch: ws.branchOrDefault(),
	}))
	if err != nil {
		return ContainerInfo{}, err
	}
	info := res.Msg.GetSandbox()
	if info == nil {
		return ContainerInfo{}, fmt.Errorf("launch returned no sandbox")
	}
	_ = s.ensureSynced(ctx, info.Name, sid, ws)
	return ContainerInfo{ContainerID: info.Name, SessionName: sid, Status: info.Phase, PodIP: info.PodIp}, nil
}

// destroyWorker deletes the sandbox through easylab (container + registry).
func (s *server) destroyWorker(ctx context.Context, id string) error {
	_, err := s.sdk.Sandbox.DeleteSandbox(ctx, connect.NewRequest(&easylabv1.DeleteSandboxRequest{Name: labelKey(id)}))
	return err
}
