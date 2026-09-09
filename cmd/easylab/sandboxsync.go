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

// syncSandboxWorkspace pushes the repo tree at the branch head into the
// sandbox container and records rev + boot id in the registry — the single
// rev-coherence write (branch head vs synced_rev vs boot_id is checked
// wherever execution is dispatched).
func (s *server) syncSandboxWorkspace(ctx context.Context, name, org, repoName, branch string) error {
	pr := s.sandboxRunner()
	if pr == nil {
		return fmt.Errorf("sandbox backend unavailable")
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
	if err := pr.SyncTar(ctx, name, "", tar); err != nil {
		return err
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
