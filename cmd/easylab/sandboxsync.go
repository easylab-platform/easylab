package main

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	workerv1 "github.com/easylab-platform/easylab-proto/worker/v1"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// syncSandboxWorkspace pushes the repo tree at the branch head into the sandbox
// worker's workspace (via the worker SyncFolder RPC) and records rev + boot id
// in the registry — the single rev-coherence write.
func (s *server) syncSandboxWorkspace(ctx context.Context, name, org, repoName, branch string) error {
	if s.k8s == nil {
		return fmt.Errorf("k8s backend unavailable")
	}
	repo, err := s.cs.OpenRepo(store.RepoRef{Namespace: org, Name: repoName})
	if err != nil {
		return fmt.Errorf("open %s/%s: %w", org, repoName, err)
	}
	ws := revision.NewWorkspace(repo)
	treeID, err := treeOfRef(ws, repo, branch)
	if err != nil {
		return fmt.Errorf("ref %s: %w", branch, err)
	}
	tar, _, err := buildTreeTar(ws, treeID)
	if err != nil {
		return err
	}
	c := &connSandbox{s: s}
	w, err := c.wc(ctx, name)
	if err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if _, err := w.SyncFolder(cctx, connect.NewRequest(&workerv1.SyncFolderRequest{
		Tarball: tar, Dest: ".", Clean: true, Rev: treeID.String(),
	})); err != nil {
		return fmt.Errorf("sync folder: %w", err)
	}
	// Record the synced rev + the worker boot id that owns this workspace.
	return s.sbx.MarkSynced(name, treeID.String(), s.workerBootID(ctx, name))
}

// workerBootID fetches the sandbox worker's boot id ("" when unreachable).
func (s *server) workerBootID(ctx context.Context, name string) string {
	c := &connSandbox{s: s}
	w, err := c.wc(ctx, name)
	if err != nil {
		return ""
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	info, err := w.Info(cctx, connect.NewRequest(&workerv1.InfoRequest{}))
	if err != nil {
		return ""
	}
	return info.Msg.BootId
}
