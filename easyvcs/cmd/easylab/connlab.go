package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	
	easylabv1 "forgejo.develop.10.199.64.20.nip.io/easylab/easylab-proto/easylab/v1"

	"connectrpc.com/connect"
	"easyvcs/internal/object"
	"easyvcs/internal/revision"
	"easyvcs/internal/store"
)

// connLab implements easylabv1connect.LabServiceHandler over the EasyVCS
// store/workspace substrate.
type connLab struct {
	s *server
}

func (c *connLab) Health(ctx context.Context, req *connect.Request[easylabv1.HealthRequest]) (*connect.Response[easylabv1.HealthResponse], error) {
	return connect.NewResponse(&easylabv1.HealthResponse{Ok: true, Version: "easylab"}), nil
}

func (c *connLab) Status(ctx context.Context, req *connect.Request[easylabv1.StatusRequest]) (*connect.Response[easylabv1.StatusResponse], error) {
	return connect.NewResponse(&easylabv1.StatusResponse{
		Ok:        true,
		Version:   "easylab",
		Db:        store.DBPath(),
		Sandboxes: 0,
	}), nil
}

func (c *connLab) ListRepos(ctx context.Context, req *connect.Request[easylabv1.ListReposRequest]) (*connect.Response[easylabv1.ListReposResponse], error) {
	repos, err := c.s.cs.List()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*easylabv1.RepoInfo, 0, len(repos))
	for _, rr := range repos {
		rp, _ := c.s.cs.OpenRepo(rr)
		if rp == nil {
			continue
		}
		meta, _ := rp.RepoMeta()
		out = append(out, &easylabv1.RepoInfo{
			Namespace: rr.Namespace,
			Name:      rr.Name,
			DefaultBranch: meta.DefaultBranch,
		})
	}
	return connect.NewResponse(&easylabv1.ListReposResponse{Repos: out}), nil
}

func (c *connLab) CreateRepo(ctx context.Context, req *connect.Request[easylabv1.CreateRepoRequest]) (*connect.Response[easylabv1.CreateRepoResponse], error) {
	_, err := c.s.cs.Create(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		if strings.Contains(err.Error(), "exists") {
			return connect.NewResponse(&easylabv1.CreateRepoResponse{Ok: true}), nil
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.CreateRepoResponse{Ok: true}), nil
}

func (c *connLab) DeleteRepo(ctx context.Context, req *connect.Request[easylabv1.DeleteRepoRequest]) (*connect.Response[easylabv1.DeleteRepoResponse], error) {
	ns, name := req.Msg.Org, req.Msg.Repo
	if err := c.s.cs.Delete(store.RepoRef{Namespace: ns, Name: name}); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&easylabv1.DeleteRepoResponse{Ok: true, Deleted: ns + "/" + name}), nil
}

func (c *connLab) EnsureRepo(ctx context.Context, req *connect.Request[easylabv1.EnsureRepoRequest]) (*connect.Response[easylabv1.EnsureRepoResponse], error) {
	_, err := c.s.cs.Create(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		if strings.Contains(err.Error(), "exists") {
			return connect.NewResponse(&easylabv1.EnsureRepoResponse{Ok: true}), nil
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.EnsureRepoResponse{Ok: true}), nil
}

func (c *connLab) EnsureOrg(ctx context.Context, req *connect.Request[easylabv1.EnsureOrgRequest]) (*connect.Response[easylabv1.EnsureOrgResponse], error) {
	// Orgs are implicit namespaces in EasyLab; an org is created lazily with
	// its first repo. Nothing to do beyond validating non-empty.
	if req.Msg.Org == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("org required"))
	}
	return connect.NewResponse(&easylabv1.EnsureOrgResponse{Ok: true}), nil
}

func (c *connLab) ForkRepo(ctx context.Context, req *connect.Request[easylabv1.ForkRepoRequest]) (*connect.Response[easylabv1.ForkRepoResponse], error) {
	src := store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo}
	dstNS := req.Msg.Org
	if req.Msg.To != "" {
		dstNS = req.Msg.To
	}
	if req.Msg.To == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("to required"))
	}
	dst, err := c.s.cs.Fork(src, store.RepoRef{Namespace: dstNS, Name: req.Msg.To})
	if err != nil {
		return nil, connect.NewError(connect.CodeAlreadyExists, err)
	}
	_ = dst
	return connect.NewResponse(&easylabv1.ForkRepoResponse{Ok: true}), nil
}

func (c *connLab) CloneRepo(ctx context.Context, req *connect.Request[easylabv1.CloneRepoRequest]) (*connect.Response[easylabv1.CloneRepoResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("CloneRepo: use mirror/pull"))
}

func (c *connLab) Tree(ctx context.Context, req *connect.Request[easylabv1.TreeRequest]) (*connect.Response[easylabv1.TreeResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	ws := revision.NewWorkspace(repo)
	treeID, err := treeOfRef(ws, repo, req.Msg.Ref)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	tree, err := ws.ReadTree(treeID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	path := req.Msg.Path
	var entries []*easylabv1.FileEntry
	for _, e := range tree.SortedEntries() {
		if path != "" && path != e.Name {
			continue
		}
		entries = append(entries, &easylabv1.FileEntry{
			Name: e.Name,
			Path: e.Name,
			Kind: strings.ToLower(e.Kind.String()),
		})
	}
	return connect.NewResponse(&easylabv1.TreeResponse{Entries: entries}), nil
}

func (c *connLab) ReadBlob(ctx context.Context, req *connect.Request[easylabv1.ReadBlobRequest]) (*connect.Response[easylabv1.ReadBlobResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	ws := revision.NewWorkspace(repo)
	treeID, err := treeOfRef(ws, repo, req.Msg.Ref)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	tree, err := ws.ReadTree(treeID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	entry, err := ws.FindEntry(tree, req.Msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	data, err := ws.ReadBlob(entry.ID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.ReadBlobResponse{
		Content: base64.StdEncoding.EncodeToString(data),
		Raw:     data,
	}), nil
}

func (c *connLab) WriteBlob(ctx context.Context, req *connect.Request[easylabv1.WriteBlobRequest]) (*connect.Response[easylabv1.WriteBlobResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	ws := revision.NewWorkspace(repo)
	meta, _ := repo.RepoMeta()
	ref := req.Msg.Ref
	if ref == "" {
		ref = meta.DefaultBranch
	}
	branch, berr := ws.GetRef(ref)
	if berr != nil || branch == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("branch %q not found", ref))
	}
	content := req.Msg.Content
	if len(req.Msg.Raw) > 0 {
		content = string(req.Msg.Raw)
	}
	_, _, err = ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{
		{Path: req.Msg.Path, Content: []byte(content)},
	}, "write "+req.Msg.Path, store.Author{Name: "easyvcs", Email: "easyvcs@example.com"}, "")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.WriteBlobResponse{Ok: true}), nil
}

func (c *connLab) Revisions(ctx context.Context, req *connect.Request[easylabv1.RevisionsRequest]) (*connect.Response[easylabv1.RevisionsResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	ws := revision.NewWorkspace(repo)
	revs, err := ws.Log()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	limit := int(req.Msg.Limit)
	if limit <= 0 {
		limit = 30
	}
	out := make([]*easylabv1.RevisionInfo, 0, len(revs))
	for i, cr := range revs {
		if i >= limit {
			break
		}
		snap, _ := repo.GetSnapshot(cr.Hash)
		out = append(out, &easylabv1.RevisionInfo{
			Rev:       cr.ID,
			Sha:       snap.RevisionHash.String(),
			Message:   snap.Description,
			Author:    snap.Author.String(),
			Timestamp: snap.CommitTime.Format(time.RFC3339),
		})
	}
	return connect.NewResponse(&easylabv1.RevisionsResponse{Revisions: out}), nil
}

func (c *connLab) Tags(ctx context.Context, req *connect.Request[easylabv1.TagsRequest]) (*connect.Response[easylabv1.TagsResponse], error) {
	out, err := c.refs(ctx, req.Msg.Org, req.Msg.Repo, store.RefTag)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(out), nil
}

func (c *connLab) Branches(ctx context.Context, req *connect.Request[easylabv1.BranchesRequest]) (*connect.Response[easylabv1.BranchesResponse], error) {
	out, err := c.refs(ctx, req.Msg.Org, req.Msg.Repo, store.RefBranch)
	if err != nil {
		return nil, err
	}
	branches := make([]*easylabv1.BranchInfo, 0, len(out.Tags))
	for _, t := range out.Tags {
		branches = append(branches, &easylabv1.BranchInfo{Name: t.Name, Sha: t.Target})
	}
	return connect.NewResponse(&easylabv1.BranchesResponse{Branches: branches}), nil
}

func (c *connLab) Diff(ctx context.Context, req *connect.Request[easylabv1.DiffRequest]) (*connect.Response[easylabv1.DiffResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	ws := revision.NewWorkspace(repo)
	id, err := resolveRevID(ws, repo, req.Msg.ChangeId)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	cr, err := repo.GetRevision(id)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	snap, err := repo.GetSnapshot(cr.Hash)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	var parent object.ID
	if len(snap.Parents) > 0 {
		parent = snap.Parents[0]
	}
	var diffs []revision.FileDiff
	if parent.IsZero() {
		diffs, err = ws.DiffContent(object.ID{}, snap.TreeID)
	} else {
		pSnap, perr := repo.GetSnapshot(parent)
		if perr != nil {
			return nil, connect.NewError(connect.CodeInternal, perr)
		}
		diffs, err = ws.DiffContent(pSnap.TreeID, snap.TreeID)
	}
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	var files []*easylabv1.DiffFile
	for _, d := range diffs {
		files = append(files, &easylabv1.DiffFile{
			Path:      d.Path,
			Diff:      d.Content,
			Additions: int32(d.AddedLines),
			Deletions: int32(d.RemovedLines),
		})
	}
	return connect.NewResponse(&easylabv1.DiffResponse{Files: files}), nil
}

func (c *connLab) Blame(ctx context.Context, req *connect.Request[easylabv1.BlameRequest]) (*connect.Response[easylabv1.BlameResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	ws := revision.NewWorkspace(repo)
	start := ""
	if req.Msg.Ref != "" {
		id, err := resolveRevID(ws, repo, req.Msg.Ref)
		if err != nil {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		start = id
	}
	annots, err := ws.Annotate(start, req.Msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	lines := make([]string, 0, len(annots))
	for _, a := range annots {
		lines = append(lines, a.Author)
	}
	return connect.NewResponse(&easylabv1.BlameResponse{Lines: lines}), nil
}

func (c *connLab) DeleteBranch(ctx context.Context, req *connect.Request[easylabv1.DeleteBranchRequest]) (*connect.Response[easylabv1.DeleteBranchResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if err := revision.NewWorkspace(repo).DeleteRef(req.Msg.Branch); err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&easylabv1.DeleteBranchResponse{Ok: true}), nil
}

func (c *connLab) CreateBranch(ctx context.Context, req *connect.Request[easylabv1.CreateBranchRequest]) (*connect.Response[easylabv1.CreateBranchResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	ws := revision.NewWorkspace(repo)
	target := req.Msg.From
	if target == "" {
		id, err := resolveRevID(ws, repo, "@")
		if err != nil {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		target = id
	}
	if _, err := ws.SetRef(req.Msg.Branch, store.RefBranch, target); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.CreateBranchResponse{Ok: true}), nil
}

func (c *connLab) FileHistory(ctx context.Context, req *connect.Request[easylabv1.FileHistoryRequest]) (*connect.Response[easylabv1.FileHistoryResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	ws := revision.NewWorkspace(repo)
	edits, err := ws.FileHistoryOpt(revision.HistoryOpt{Start: req.Msg.Ref, Desc: true}, req.Msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	var out []*easylabv1.CommitInfo
	for _, e := range edits {
		out = append(out, &easylabv1.CommitInfo{
			ChangeId:  e.RevisionID,
			CommitId:  e.RevisionHash.String(),
			Timestamp: e.Timestamp.Format(time.RFC3339),
		})
	}
	return connect.NewResponse(&easylabv1.FileHistoryResponse{Commits: out}), nil
}

func (c *connLab) Log(ctx context.Context, req *connect.Request[easylabv1.LogRequest]) (*connect.Response[easylabv1.LogResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	ws := revision.NewWorkspace(repo)
	revs, err := ws.Log()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	limit := int(req.Msg.Limit)
	if limit <= 0 {
		limit = 30
	}
	out := make([]*easylabv1.CommitInfo, 0, len(revs))
	for i, cr := range revs {
		if i >= limit {
			break
		}
		snap, _ := repo.GetSnapshot(cr.Hash)
		agg := &easylabv1.CommitInfo{
			ChangeId:  cr.ID,
			CommitId:  cr.Hash.String(),
			Timestamp: snap.CommitTime.Format(time.RFC3339),
		}
		if snap != nil {
			agg.Author = snap.Author.String()
			agg.Message = snap.Description
		}
		out = append(out, agg)
	}
	return connect.NewResponse(&easylabv1.LogResponse{Commits: out}), nil
}

// refs returns RefInfos for a given ref kind.
func (c *connLab) refs(ctx context.Context, org, repo string, kind store.RefKind) (*easylabv1.TagsResponse, error) {
	rr, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: org, Name: repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	found, err := revision.NewWorkspace(rr).ListRefs()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	var out []*easylabv1.TagInfo
	for _, rf := range found {
		if rf.Kind != kind {
			continue
		}
		out = append(out, &easylabv1.TagInfo{Name: rf.Name, Target: rf.Target})
	}
	return &easylabv1.TagsResponse{Tags: out}, nil
}

var _ = errors.New
var _ = time.Now
