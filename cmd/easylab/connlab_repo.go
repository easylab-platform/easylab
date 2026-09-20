package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"connectrpc.com/connect"

	artifactkit "github.com/easylab-platform/artifact/core"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// ---- repository metadata (RPC form of the removed PATCH /api/v1/repo/...) ----

func (c *connLab) UpdateRepo(ctx context.Context, req *connect.Request[easylabv1.UpdateRepoRequest]) (*connect.Response[easylabv1.UpdateRepoResponse], error) {
	// Changing hosting metadata (visibility/description/default branch) is a
	// shared, externally-visible repo-level change: owner+.
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanPush, "updating repository metadata"); err != nil {
		return nil, err
	}
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	meta, err := repo.RepoMeta()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.Description != nil {
		meta.Description = *req.Msg.Description
	}
	if req.Msg.Visibility != nil {
		if v := *req.Msg.Visibility; v != "public" && v != "private" {
			return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("visibility must be public|private"))
		}
		meta.Visibility = *req.Msg.Visibility
	}
	if req.Msg.DefaultBranch != nil && *req.Msg.DefaultBranch != "" {
		meta.DefaultBranch = *req.Msg.DefaultBranch
	}
	if err := repo.UpdateRepoMeta(meta); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.UpdateRepoResponse{Ok: true}), nil
}

// ---- tags ----

func (c *connLab) SetTag(ctx context.Context, req *connect.Request[easylabv1.SetTagRequest]) (*connect.Response[easylabv1.SetTagResponse], error) {
	// Tagging is on par with merging (maintainer+); it does not rewrite content.
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanMerge, "setting a tag"); err != nil {
		return nil, err
	}
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, err
	}
	if req.Msg.Name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name required"))
	}
	ws := revision.NewWorkspace(repo)
	target := req.Msg.Target
	if target == "" || target == "@" {
		id, rerr := resolveRevID(ws, repo, "@")
		if rerr != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, rerr)
		}
		target = id
	}
	if _, err := ws.SetRef(req.Msg.Name, store.RefTag, target); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&easylabv1.SetTagResponse{Ok: true}), nil
}

func (c *connLab) DeleteTag(ctx context.Context, req *connect.Request[easylabv1.DeleteTagRequest]) (*connect.Response[easylabv1.DeleteTagResponse], error) {
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanMerge, "deleting a tag"); err != nil {
		return nil, err
	}
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, err
	}
	if err := revision.NewWorkspace(repo).DeleteRef(req.Msg.Name); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&easylabv1.DeleteTagResponse{Ok: true}), nil
}

// ---- atomic multi-file write ----

// WriteFiles commits one or more file changes onto a branch in a single
// revision. It is the RPC form of the removed POST /api/v1/repo/.../files.
func (c *connLab) WriteFiles(ctx context.Context, req *connect.Request[easylabv1.WriteFilesRequest]) (*connect.Response[easylabv1.WriteFilesResponse], error) {
	// A direct branch write is owner-only (developers propose via fork+MR).
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanPush, "writing files"); err != nil {
		return nil, err
	}
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, err
	}
	if len(req.Msg.Changes) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("at least one change required"))
	}
	revID, changeID, err := applyFileChanges(repo, req.Msg.Ref, req.Msg.Message, req.Msg.NewCommit, req.Msg.Changes)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&easylabv1.WriteFilesResponse{Ok: true, RevisionId: revID, ChangeId: changeID}), nil
}

// applyFileChanges is the shared write core: resolve the branch, build the
// parent, commit the change set, and move the branch ref. It mirrors the
// removed REST handler so the agent extension keeps real delete semantics.
func applyFileChanges(repo *store.Repo, ref, message string, newCommit bool, changes []*easylabv1.FileChange) (revID, changeID string, err error) {
	ws := revision.NewWorkspace(repo)
	isDefault := false
	if ref == "" {
		meta, merr := repo.RepoMeta()
		if merr != nil {
			return "", "", merr
		}
		ref = meta.DefaultBranch
		isDefault = true
	}
	branch, berr := ws.GetRef(ref)
	if berr != nil || branch == nil || branch.Kind != store.RefBranch {
		if !isDefault {
			return "", "", fmt.Errorf("branch %q not found; every write must target a branch", ref)
		}
		branch = &store.Ref{Name: ref, Kind: store.RefBranch}
	}

	var parentID object.ID
	if newCommit {
		if branch.Target != "" {
			parentID, err = resolveRefAny(ws, repo, branch.Name)
			if err != nil {
				return "", "", err
			}
		}
	} else {
		// Amend onto the branch tip (stable revision id) unless it has no tip.
		if branch.Target != "" {
			parentID = object.ID{}
			revID, changeID, err = commitChanges(ws, parentID, branch.Target, message, changes)
			if err != nil {
				return "", "", err
			}
			if _, serr := ws.SetRef(branch.Name, store.RefBranch, changeID); serr != nil {
				return "", "", serr
			}
			return revID, changeID, nil
		}
	}
	revID, changeID, err = commitChanges(ws, parentID, "", message, changes)
	if err != nil {
		return "", "", err
	}
	if _, serr := ws.SetRef(branch.Name, store.RefBranch, changeID); serr != nil {
		return "", "", serr
	}
	return revID, changeID, nil
}

// commitChanges maps proto FileChange to the revision layer and commits.
func commitChanges(ws *revision.Workspace, parent object.ID, revisionIDOverride, message string, changes []*easylabv1.FileChange) (string, string, error) {
	specs := make([]revision.FileChangeSpec, 0, len(changes))
	for _, ch := range changes {
		content := []byte(ch.Content)
		if len(ch.Raw) > 0 {
			content = ch.Raw
		}
		specs = append(specs, revision.FileChangeSpec{Path: ch.Path, Content: content, Delete: ch.Delete})
	}
	_, rev, err := ws.CommitFromChanges(parent, specs, message, store.Author{Name: "easyvcs", Email: "easyvcs@example.com"}, revisionIDOverride)
	if err != nil {
		return "", "", err
	}
	return rev.ID, rev.ID, nil
}

// ---- history editing (drop/revert/resolve/squash/rebase-many) ----

func (c *connLab) Drop(ctx context.Context, req *connect.Request[easylabv1.DropRequest]) (*connect.Response[easylabv1.DropResponse], error) {
	repo, ws, err := c.requireHistoryEdit(ctx, req.Msg.Org, req.Msg.Repo, "dropping a revision")
	if err != nil {
		return nil, err
	}
	id, err := resolveRevID(ws, repo, req.Msg.Rev)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if _, err := ws.Drop(id); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&easylabv1.DropResponse{Ok: true, RevisionId: id}), nil
}

func (c *connLab) Revert(ctx context.Context, req *connect.Request[easylabv1.RevertRequest]) (*connect.Response[easylabv1.RevertResponse], error) {
	repo, ws, err := c.requireHistoryEdit(ctx, req.Msg.Org, req.Msg.Repo, "reverting a revision")
	if err != nil {
		return nil, err
	}
	id, err := resolveRevID(ws, repo, req.Msg.Rev)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	var onto object.ID
	if req.Msg.Target != "" {
		if onto, err = resolveRefAny(ws, repo, req.Msg.Target); err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
	}
	snap, ch, err := ws.Revert(id, onto)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&easylabv1.RevertResponse{Ok: true, RevisionId: ch.ID, ChangeId: snap.RevisionID}), nil
}

func (c *connLab) Resolve(ctx context.Context, req *connect.Request[easylabv1.ResolveRequest]) (*connect.Response[easylabv1.ResolveResponse], error) {
	// Resolving a conflict rewrites a file: it is a direct content write.
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanPush, "resolving a conflict")
	if err != nil {
		return nil, err
	}
	if req.Msg.Path == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("path required"))
	}
	_, changeID, err := applyFileChanges(repo, "", "resolve "+req.Msg.Path, true, []*easylabv1.FileChange{
		{Path: req.Msg.Path, Content: req.Msg.Content},
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&easylabv1.ResolveResponse{Ok: true, ChangeId: changeID}), nil
}

func (c *connLab) Squash(ctx context.Context, req *connect.Request[easylabv1.SquashRequest]) (*connect.Response[easylabv1.SquashResponse], error) {
	repo, ws, err := c.requireHistoryEdit(ctx, req.Msg.Org, req.Msg.Repo, "squashing a revision")
	if err != nil {
		return nil, err
	}
	id, err := resolveRevID(ws, repo, req.Msg.Rev)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	snap, ch, err := ws.Squash(id)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&easylabv1.SquashResponse{Ok: true, RevisionId: ch.ID, ChangeId: snap.RevisionID}), nil
}

func (c *connLab) RebaseMany(ctx context.Context, req *connect.Request[easylabv1.RebaseManyRequest]) (*connect.Response[easylabv1.RebaseManyResponse], error) {
	repo, ws, err := c.requireHistoryEdit(ctx, req.Msg.Org, req.Msg.Repo, "rebasing revisions")
	if err != nil {
		return nil, err
	}
	if len(req.Msg.Revs) == 0 || req.Msg.Onto == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("revs and onto required"))
	}
	ontoID, err := resolveRefAny(ws, repo, req.Msg.Onto)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	revIDs := make([]string, 0, len(req.Msg.Revs))
	for _, r := range req.Msg.Revs {
		id, rerr := resolveRevID(ws, repo, r)
		if rerr != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, rerr)
		}
		revIDs = append(revIDs, id)
	}
	if _, err := ws.RebaseMany(revIDs, ontoID); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&easylabv1.RebaseManyResponse{Ok: true}), nil
}

// requireHistoryEdit gates a history rewrite (owner-only) and opens the repo.
func (c *connLab) requireHistoryEdit(ctx context.Context, org, repo, action string) (*store.Repo, *revision.Workspace, error) {
	if err := c.s.requireRepoAction(ctx, org, repo, repoCanPush, action); err != nil {
		return nil, nil, err
	}
	r, err := c.s.openRepoAuthorized(ctx, org, repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, nil, err
	}
	return r, revision.NewWorkspace(r), nil
}

// ---- releases (write side) ----

func (c *connLab) CreateRelease(ctx context.Context, req *connect.Request[easylabv1.CreateReleaseRequest]) (*connect.Response[easylabv1.CreateReleaseResponse], error) {
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanMerge, "creating a release"); err != nil {
		return nil, err
	}
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, err
	}
	if req.Msg.Tag == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("tag required"))
	}
	repoName := releaseRepository(repo)
	meta := labReleaseMeta{
		Name: req.Msg.Name, Description: req.Msg.Description, RevisionID: req.Msg.Target,
		Draft: req.Msg.Draft, Prerelease: req.Msg.Prerelease, Created: time.Now().UTC(),
	}
	metaBytes, _ := json.Marshal(meta)
	art := artifactkit.Artifact{Format: "generic", Repository: repoName, Version: req.Msg.Tag, Proprietary: metaBytes}
	if err := c.s.registry.Meta.Put(ctx, art); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.CreateReleaseResponse{Ok: true, Tag: req.Msg.Tag}), nil
}

func (c *connLab) DeleteRelease(ctx context.Context, req *connect.Request[easylabv1.DeleteReleaseRequest]) (*connect.Response[easylabv1.DeleteReleaseResponse], error) {
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanMerge, "deleting a release"); err != nil {
		return nil, err
	}
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, err
	}
	repoName := releaseRepository(repo)
	art, gerr := c.s.registry.Meta.Get(ctx, "generic", repoName, req.Msg.Tag)
	if gerr != nil {
		return nil, connect.NewError(connect.CodeNotFound, gerr)
	}
	for _, b := range art.Blobs {
		_ = c.s.registry.Blobs.Delete(ctx, b.Digest)
	}
	if err := c.s.registry.Meta.Delete(ctx, "generic", repoName, req.Msg.Tag); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.DeleteReleaseResponse{Ok: true}), nil
}

func (c *connLab) UploadReleaseAsset(ctx context.Context, req *connect.Request[easylabv1.UploadReleaseAssetRequest]) (*connect.Response[easylabv1.UploadReleaseAssetResponse], error) {
	if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanMerge, "uploading a release asset"); err != nil {
		return nil, err
	}
	repo, err := c.s.openRepoAuthorized(ctx, req.Msg.Org, req.Msg.Repo, repoCanRead, "reading a repository")
	if err != nil {
		return nil, err
	}
	repoName := releaseRepository(repo)
	if _, gerr := c.s.registry.Meta.Get(ctx, "generic", repoName, req.Msg.Tag); gerr != nil {
		return nil, connect.NewError(connect.CodeNotFound, gerr)
	}
	name := req.Msg.Name
	if name == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("name required"))
	}
	data := req.Msg.Data
	digest := artifactkit.DigestOf(data)
	if _, _, err := c.s.registry.Blobs.Put(ctx, bytes.NewReader(data), digest); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	art, _ := c.s.registry.Meta.Get(ctx, "generic", repoName, req.Msg.Tag)
	kept := art.Blobs[:0]
	for _, b := range art.Blobs {
		if b.Name != name {
			kept = append(kept, b)
		}
	}
	art.Blobs = append(kept, artifactkit.Descriptor{Digest: digest, Size: int64(len(data)), Name: name})
	if err := c.s.registry.Meta.Put(ctx, art); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.UploadReleaseAssetResponse{Ok: true, Name: name}), nil
}
