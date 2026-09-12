package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"time"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"

	"connectrpc.com/connect"
	artifactkit "github.com/easylab-platform/artifact/core"
	"github.com/easylab-platform/easyvcs/mirror"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// DeleteOrg removes every repository under a namespace/organization.
func (c *connLab) DeleteOrg(ctx context.Context, req *connect.Request[easylabv1.DeleteOrgRequest]) (*connect.Response[easylabv1.DeleteOrgResponse], error) {
	ns := req.Msg.Org
	repos, err := c.s.cs.List()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	deleted := 0
	for _, rr := range repos {
		if rr.Namespace != ns {
			continue
		}
		if err := c.s.cs.Delete(rr); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		deleted++
	}
	return connect.NewResponse(&easylabv1.DeleteOrgResponse{Ok: true}), nil
}

// ListReleases returns the generic-artifact releases for a repo.
func (c *connLab) ListReleases(ctx context.Context, req *connect.Request[easylabv1.ListReleasesRequest]) (*connect.Response[easylabv1.ListReleasesResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	repoName := releaseRepository(repo)
	versions, err := c.s.registry.Meta.ListVersions(ctx, "generic", repoName)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*easylabv1.ReleaseView, 0, len(versions))
	for _, v := range versions {
		art, err := c.s.registry.Meta.Get(ctx, "generic", repoName, v)
		if err != nil {
			continue
		}
		out = append(out, releaseViewFromArtifact(art))
	}
	return connect.NewResponse(&easylabv1.ListReleasesResponse{Releases: out}), nil
}

// DownloadReleaseAsset returns the bytes of one release asset.
func (c *connLab) DownloadReleaseAsset(ctx context.Context, req *connect.Request[easylabv1.DownloadReleaseAssetRequest]) (*connect.Response[easylabv1.DownloadReleaseAssetResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	repoName := releaseRepository(repo)
	art, err := c.s.registry.Meta.Get(ctx, "generic", repoName, req.Msg.Tag)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	var chosen artifactkit.Descriptor
	for _, b := range art.Blobs {
		if b.Name == req.Msg.Name {
			chosen = b
			break
		}
	}
	if chosen.IsEmpty() {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	rd, err := c.s.registry.Blobs.Open(ctx, chosen.Digest)
	if err != nil || rd == nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	defer rd.Close()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rd); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.DownloadReleaseAssetResponse{
		Data:        buf.Bytes(),
		Name:        chosen.Name,
		ContentType: chosen.MediaType,
	}), nil
}

// Archive returns a tar.gz of the repository tree at a rev/tag.
func (c *connLab) Archive(ctx context.Context, req *connect.Request[easylabv1.ArchiveRequest]) (*connect.Response[easylabv1.ArchiveResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	ws := revision.NewWorkspace(repo)
	treeID, err := treeOfRef(ws, repo, req.Msg.Ref)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	tmp, err := os.MkdirTemp("", "easyvcs-archive-*")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	defer os.RemoveAll(tmp)
	if err := ws.Materialize(treeID, tmp); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err = filepath.Walk(tmp, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(tmp, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		hdr, hErr := tar.FileInfoHeader(info, "")
		if hErr != nil {
			return hErr
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
			return tw.WriteHeader(hdr)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, fErr := os.Open(path)
		if fErr != nil {
			return fErr
		}
		defer f.Close()
		_, cErr := io.Copy(tw, f)
		return cErr
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := tw.Close(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if err := gz.Close(); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.ArchiveResponse{
		Data:     buf.Bytes(),
		Filename: repo.Name + "-" + req.Msg.Ref + ".tar.gz",
	}), nil
}

// GetMirror returns the repo's mirror metadata (pull URL for mirror repos;
// push URL from the single push-mirror).
func (c *connLab) GetMirror(ctx context.Context, req *connect.Request[easylabv1.GetMirrorRequest]) (*connect.Response[easylabv1.GetMirrorResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	meta, err := repo.RepoMeta()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	cfg := &easylabv1.MirrorCfg{}
	if meta.Kind == "mirror" {
		cfg.PullUrl = meta.MirrorURL
	}
	pushes, err := repo.ListPushMirrors()
	if err == nil && len(pushes) > 0 {
		cfg.PushUrl = pushes[0].URL
	}
	return connect.NewResponse(&easylabv1.GetMirrorResponse{Mirror: cfg}), nil
}

// SetMirror configures a mirror (pull for mirror repos, push-mirror for normal).
func (c *connLab) SetMirror(ctx context.Context, req *connect.Request[easylabv1.SetMirrorRequest]) (*connect.Response[easylabv1.SetMirrorResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	meta, err := repo.RepoMeta()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if req.Msg.PullUrl != "" {
		meta.Kind = "mirror"
		meta.MirrorURL = req.Msg.PullUrl
		if err := repo.UpdateMirrorMeta(meta); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	if req.Msg.PushUrl != "" || req.Msg.PushSecret != "" {
		name := "default"
		m := &store.PushMirror{Name: name, URL: req.Msg.PushUrl, Branch: meta.DefaultBranch, Token: req.Msg.PushSecret}
		if err := repo.PutPushMirror(m); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	return connect.NewResponse(&easylabv1.SetMirrorResponse{Ok: true}), nil
}

// DeleteMirror removes a repo's mirror configuration.
func (c *connLab) DeleteMirror(ctx context.Context, req *connect.Request[easylabv1.DeleteMirrorRequest]) (*connect.Response[easylabv1.DeleteMirrorResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	meta, err := repo.RepoMeta()
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if meta.Kind == "mirror" {
		meta.Kind = "normal"
		meta.MirrorURL = ""
		meta.MirrorBranch = ""
		meta.MirrorToken = ""
		if err := repo.UpdateMirrorMeta(meta); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}
	pushes, _ := repo.ListPushMirrors()
	for _, m := range pushes {
		_ = repo.DeletePushMirror(m.Name)
	}
	return connect.NewResponse(&easylabv1.DeleteMirrorResponse{Ok: true}), nil
}

// SyncMirror dispatches by kind: pull (mirror repo) or push (normal repo).
func (c *connLab) SyncMirror(ctx context.Context, req *connect.Request[easylabv1.SyncMirrorRequest]) (*connect.Response[easylabv1.SyncMirrorResponse], error) {
	repo, err := c.s.cs.OpenRepo(store.RepoRef{Namespace: req.Msg.Org, Name: req.Msg.Repo})
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if repo.IsMirror() {
		meta, err := repo.RepoMeta()
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		rev, err := mirror.Pull(ctx, repo, mirror.PullConfig{
			URL: meta.MirrorURL, Branch: meta.MirrorBranch, Token: meta.MirrorToken,
		})
		if err != nil {
			_ = repo.TouchMirrorSync("", time.Now().UTC().UnixMilli(), err.Error())
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		_ = repo.TouchMirrorSync(rev.ID, time.Now().UTC().UnixMilli(), "")
		return connect.NewResponse(&easylabv1.SyncMirrorResponse{Ok: true, Error: ""}), nil
	}
	// push
	name := req.Msg.Body["name"]
	if name == "" {
		name = "default"
	}
	m, err := repo.GetPushMirror(name)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	res, err := mirror.Push(ctx, repo, mirror.PushTarget{
		Name: m.Name, URL: m.URL, Branch: m.Branch, Token: m.Token, LastRev: m.LastRev,
	})
	if err != nil {
		_ = repo.UpdatePushMirrorSync(m.Name, m.LastRev, err.Error())
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	_ = repo.UpdatePushMirrorSync(m.Name, res.RevID, "")
	return connect.NewResponse(&easylabv1.SyncMirrorResponse{Ok: true, UpdatedBranches: res.RevID}), nil
}

// releaseViewFromArtifact maps an artifactkit.Artifact to a ReleaseView.
func releaseViewFromArtifact(a artifactkit.Artifact) *easylabv1.ReleaseView {
	view := &easylabv1.ReleaseView{Tag: a.Version}
	var meta labReleaseMeta
	if len(a.Proprietary) > 0 {
		_ = json.Unmarshal(a.Proprietary, &meta)
	}
	view.Name = meta.Name
	view.Description = meta.Description
	view.RevisionId = meta.RevisionID
	view.Draft = meta.Draft
	view.Prerelease = meta.Prerelease
	view.Created = meta.Created.UTC().Format(time.RFC3339)
	for _, b := range a.Blobs {
		view.Assets = append(view.Assets, &easylabv1.ReleaseAssetView{
			Name:        b.Name,
			Size:        b.Size,
			Digest:      b.Digest,
			ContentType: b.MediaType,
		})
	}
	return view
}
