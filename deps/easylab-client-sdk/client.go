// Package easylabsdk provides a strong-typed client for the EasyLab service.
//
// EasyLab exposes three Connect services (LabService, OpsService,
// RegistryService) generated from easylab/v1/easylab.proto by buf. This SDK
// adds Bearer auth + ergonomic wrappers so ext servers (and any consumer)
// never hard-code REST paths. It only talks to easylab; it does NOT talk to
// the agent (use the separate abc agent SDK for that).
package easylabsdk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easylab-proto/easylab/v1/easylabv1connect"
)

// Client is a thin Easylab client exposing three typed service surfaces.
type Client struct {
	base     string
	Lab      easylabv1connect.LabServiceClient
	Ops      easylabv1connect.OpsServiceClient
	Registry easylabv1connect.RegistryServiceClient
}

// New builds an easylab client. token, when non-empty, is sent as Bearer.
// baseURL is protocol+host (no trailing slash).
func New(baseURL, token string) *Client {
	if token == "" {
		token = "devtoken"
	}
	inter := authInterceptor(token)
	base := trimSlash(baseURL)
	return &Client{
		base:     base,
		Lab:      easylabv1connect.NewLabServiceClient(http.DefaultClient, base, connect.WithInterceptors(inter)),
		Ops:      easylabv1connect.NewOpsServiceClient(http.DefaultClient, base, connect.WithInterceptors(inter)),
		Registry: easylabv1connect.NewRegistryServiceClient(http.DefaultClient, base, connect.WithInterceptors(inter)),
	}
}

func authInterceptor(token string) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			req.Header().Set("Authorization", "Bearer "+token)
			return next(ctx, req)
		}
	})
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// ---- repo helpers (bookmark semantics are gone; it's branches now) ----

// RepoInfo is the easylab repo view.
type RepoInfo struct {
	Namespace, Name, DefaultBranch string
	Sha                            string
}

// EnsureRepo creates org/repo (idempotent: already-exists is success).
func (c *Client) EnsureRepo(ctx context.Context, org, repo string) error {
	_, err := c.Lab.EnsureRepo(ctx, connect.NewRequest(&easylabv1.EnsureRepoRequest{Org: org, Repo: repo}))
	if err == nil || IsExists(err) {
		return nil
	}
	return errDownstream("easylab", err)
}

// EnsureOrg creates an org namespace (idempotent).
func (c *Client) EnsureOrg(ctx context.Context, org string) error {
	_, err := c.Lab.EnsureOrg(ctx, connect.NewRequest(&easylabv1.EnsureOrgRequest{Org: org}))
	if err == nil || IsExists(err) {
		return nil
	}
	return errDownstream("easylab", err)
}

// ListRepos returns all repos (namespace + name + default branch).
func (c *Client) ListRepos(ctx context.Context) ([]RepoInfo, error) {
	res, err := c.Lab.ListRepos(ctx, connect.NewRequest(&easylabv1.ListReposRequest{}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	out := make([]RepoInfo, 0, len(res.Msg.GetRepos()))
	for _, r := range res.Msg.GetRepos() {
		out = append(out, RepoInfo{
			Namespace:     r.GetNamespace(),
			Name:          r.GetName(),
			DefaultBranch: r.GetDefaultBranch(),
			Sha:           r.GetSha(),
		})
	}
	return out, nil
}

// Branches lists a repo's branch names+sha.
func (c *Client) Branches(ctx context.Context, org, repo string) ([]*easylabv1.BranchInfo, error) {
	res, err := c.Lab.Branches(ctx, connect.NewRequest(&easylabv1.BranchesRequest{Org: org, Repo: repo}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetBranches(), nil
}

// Tags lists a repo's tags.
func (c *Client) Tags(ctx context.Context, org, repo string) ([]*easylabv1.TagInfo, error) {
	res, err := c.Lab.Tags(ctx, connect.NewRequest(&easylabv1.TagsRequest{Org: org, Repo: repo}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetTags(), nil
}

// Revisions lists a repo's revisions (commits).
func (c *Client) Revisions(ctx context.Context, org, repo, ref string, limit int32) ([]*easylabv1.RevisionInfo, error) {
	res, err := c.Lab.Revisions(ctx, connect.NewRequest(&easylabv1.RevisionsRequest{Org: org, Repo: repo, Ref: ref, Limit: limit}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetRevisions(), nil
}

// CreateBranch creates a branch from a target revision ref (idempotent).
func (c *Client) CreateBranch(ctx context.Context, org, repo, branch, from string) error {
	_, err := c.Lab.CreateBranch(ctx, connect.NewRequest(&easylabv1.CreateBranchRequest{Org: org, Repo: repo, Branch: branch, From: from}))
	if err == nil || IsExists(err) {
		return nil
	}
	return errDownstream("easylab", err)
}

// DeleteBranch removes a branch (idempotent when absent).
func (c *Client) DeleteBranch(ctx context.Context, org, repo, branch string) error {
	_, err := c.Lab.DeleteBranch(ctx, connect.NewRequest(&easylabv1.DeleteBranchRequest{Org: org, Repo: repo, Branch: branch}))
	if err == nil || IsNotFound(err) {
		return nil
	}
	return errDownstream("easylab", err)
}

// ReadBlob reads a file's bytes at a ref (branch/sha/revision).
func (c *Client) ReadBlob(ctx context.Context, org, repo, path, ref string) ([]byte, error) {
	res, err := c.Lab.ReadBlob(ctx, connect.NewRequest(&easylabv1.ReadBlobRequest{Org: org, Repo: repo, Path: path, Ref: ref}))
	if err != nil {
		if IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, errDownstream("easylab", err)
	}
	if len(res.Msg.GetRaw()) > 0 {
		return res.Msg.GetRaw(), nil
	}
	return []byte(res.Msg.GetContent()), nil
}

// WriteBlob writes a single file onto a branch, committing a change.
// Returns (ok, errorMsg).
func (c *Client) WriteBlob(ctx context.Context, org, repo, branch, path, content, message string) (bool, error) {
	res, err := c.Lab.WriteBlob(ctx, connect.NewRequest(&easylabv1.WriteBlobRequest{
		Org: org, Repo: repo, Ref: branch, Path: path, Content: content, Message: message,
	}))
	if err != nil {
		return false, errDownstream("easylab", err)
	}
	return res.Msg.GetOk(), errors.New(res.Msg.GetError())
}

// Log lists commits for a repo at ref.
func (c *Client) Log(ctx context.Context, org, repo, ref string, limit int32) ([]*easylabv1.CommitInfo, error) {
	res, err := c.Lab.Log(ctx, connect.NewRequest(&easylabv1.LogRequest{Org: org, Repo: repo, Ref: ref, Limit: limit}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetCommits(), nil
}

// Diff returns per-file diffs for a change relative to its parent.
func (c *Client) Diff(ctx context.Context, org, repo, changeID, path string) ([]*easylabv1.DiffFile, error) {
	res, err := c.Lab.Diff(ctx, connect.NewRequest(&easylabv1.DiffRequest{Org: org, Repo: repo, ChangeId: changeID, Path: path}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetFiles(), nil
}

// ---- ops ----

// Build triggers a container image build, returning the task id.
func (c *Client) Build(ctx context.Context, req *easylabv1.BuildRequest) (string, error) {
	res, err := c.Ops.Build(ctx, connect.NewRequest(req))
	if err != nil {
		return "", errDownstream("easylab", err)
	}
	return res.Msg.GetTaskId(), nil
}

// Run triggers a package publish run, returning the task id.
func (c *Client) Run(ctx context.Context, req *easylabv1.RunRequest) (string, error) {
	res, err := c.Ops.Run(ctx, connect.NewRequest(req))
	if err != nil {
		return "", errDownstream("easylab", err)
	}
	return res.Msg.GetTaskId(), nil
}

// ListServices lists services/deployments.
func (c *Client) ListServices(ctx context.Context, org, repo, namespace string) ([]*easylabv1.ServiceInfo, error) {
	res, err := c.Ops.ListServices(ctx, connect.NewRequest(&easylabv1.ListServicesRequest{Org: org, Repo: repo, Namespace: namespace}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetServices(), nil
}

// GetService returns a single service (+ its pods in the same response).
func (c *Client) GetService(ctx context.Context, name string) (*easylabv1.GetServiceResponse, error) {
	res, err := c.Ops.GetService(ctx, connect.NewRequest(&easylabv1.GetServiceRequest{Name: name}))
	if err != nil {
		if IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, errDownstream("easylab", err)
	}
	return res.Msg, nil
}

// SandboxExec runs a command in a session sandbox.
func (c *Client) SandboxExec(ctx context.Context, name, command string) (*easylabv1.SandboxExecResponse, error) {
	res, err := c.Ops.SandboxExec(ctx, connect.NewRequest(&easylabv1.SandboxExecRequest{Name: name, Command: command}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg, nil
}

// ListTasks lists build/run tasks.
func (c *Client) ListTasks(ctx context.Context) ([]*easylabv1.TaskEntry, error) {
	res, err := c.Ops.ListTasks(ctx, connect.NewRequest(&easylabv1.ListTasksRequest{}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetTasks(), nil
}

// GetTask returns a single task.
func (c *Client) GetTask(ctx context.Context, id string) (*easylabv1.TaskEntry, error) {
	res, err := c.Ops.GetTask(ctx, connect.NewRequest(&easylabv1.GetTaskRequest{Id: id}))
	if err != nil {
		if IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetTask(), nil
}

// TaskLog subscribes to a task's log stream.
func (c *Client) TaskLog(ctx context.Context, id string) (*connect.ServerStreamForClient[easylabv1.TaskLogResponse], error) {
	return c.Ops.TaskLog(ctx, connect.NewRequest(&easylabv1.TaskLogRequest{Id: id}))
}

// ---- registry ----

// ListPackageTypes lists package protocol types.
func (c *Client) ListPackageTypes(ctx context.Context) ([]*easylabv1.PackageTypeEntry, error) {
	res, err := c.Registry.ListPackageTypes(ctx, connect.NewRequest(&easylabv1.ListPackageTypesRequest{}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetPackages(), nil
}

// PackageVersions lists versions for a package type+name.
func (c *Client) PackageVersions(ctx context.Context, typ, name string) ([]*easylabv1.PackageVersion, error) {
	res, err := c.Registry.PackageVersions(ctx, connect.NewRequest(&easylabv1.PackageVersionsRequest{Type: typ, Name: name}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetVersions(), nil
}

// ListPublishSpecs lists publish protocol specs.
func (c *Client) ListPublishSpecs(ctx context.Context) ([]*easylabv1.PublishSpec, error) {
	res, err := c.Registry.ListPublishSpecs(ctx, connect.NewRequest(&easylabv1.ListPublishSpecsRequest{}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetSpecs(), nil
}

// ---- errors ----

var (
	ErrNotFound = errors.New("not found")
	ErrExists   = errors.New("already exists")
)

func IsNotFound(err error) bool { return connect.CodeOf(err) == connect.CodeNotFound }
func IsExists(err error) bool {
	c := connect.CodeOf(err)
	return c == connect.CodeAlreadyExists || c == connect.CodeInvalidArgument
}

func errDownstream(svc string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", svc, err)
}

// ---- ext/code helpers (built on the typed RPCs above) ----

// FileEntry is a directory listing entry.
type FileEntry struct {
	Name, Path, Kind string
	Size             int32
}

// ListReposTreeEntries lists a repo's tree entries at a ref/path.
func (c *Client) ListReposTreeEntries(ctx context.Context, org, repo, ref, path string) ([]FileEntry, error) {
	res, err := c.Lab.Tree(ctx, connect.NewRequest(&easylabv1.TreeRequest{Org: org, Repo: repo, Ref: ref, Path: path}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	out := make([]FileEntry, 0, len(res.Msg.GetEntries()))
	for _, e := range res.Msg.GetEntries() {
		out = append(out, FileEntry{Name: e.GetName(), Path: e.GetPath(), Kind: e.GetKind(), Size: e.GetSize()})
	}
	return out, nil
}

// ReadBlobText reads a file at ref as UTF-8 text.
func (c *Client) ReadBlobText(ctx context.Context, org, repo, path, ref string) (string, error) {
	b, err := c.ReadBlob(ctx, org, repo, path, ref)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// RevisionDiff diffs a single rev (REST-only; uses Diff on that change).
func (c *Client) RevisionDiff(ctx context.Context, org, repo, rev string) (string, error) {
	files, err := c.Diff(ctx, org, repo, rev, "")
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, f := range files {
		sb.WriteString(f.GetDiff())
	}
	return sb.String(), nil
}

// Blame returns per-line annotations (revision id + content) for a file.
func (c *Client) Blame(ctx context.Context, org, repo, path, ref string) ([]map[string]any, error) {
	lines, err := c.Lab.Blame(ctx, connect.NewRequest(&easylabv1.BlameRequest{Org: org, Repo: repo, Path: path, Ref: ref}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	out := make([]map[string]any, 0, len(lines.Msg.GetLines()))
	for i, l := range lines.Msg.GetLines() {
		out = append(out, map[string]any{"_rev": l, "_content": "", "_line": i + 1})
	}
	return out, nil
}

// FileHistory lists a file's edit history.
func (c *Client) FileHistory(ctx context.Context, org, repo, path, ref string) ([]map[string]any, error) {
	res, err := c.Lab.FileHistory(ctx, connect.NewRequest(&easylabv1.FileHistoryRequest{Org: org, Repo: repo, Path: path, Ref: ref}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	out := make([]map[string]any, 0, len(res.Msg.GetCommits()))
	for _, c2 := range res.Msg.GetCommits() {
		out = append(out, map[string]any{"_rev": c2.GetChangeId(), "_status": c2.GetMessage()})
	}
	return out, nil
}

// RawSearch is the REST fallback for the grep tool (no typed RPC yet).
func (c *Client) RawSearch(ctx context.Context, org, repo, ref, q string) ([]map[string]any, error) {
	return nil, errDownstream("easylab", fmt.Errorf("search: no typed RPC"))
}

// ---- search / graph / compare / rebase / sync (v0.3.0 surface) ----

// Search greps a repo at ref for q, returning matching paths.
func (c *Client) Search(ctx context.Context, org, repo, ref, q string) ([]string, error) {
	res, err := c.Lab.Search(ctx, connect.NewRequest(&easylabv1.SearchRequest{Org: org, Repo: repo, Ref: ref, Q: q}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetMatches(), nil
}

// Graph returns the revision DAG (topological, parents-first).
func (c *Client) Graph(ctx context.Context, org, repo string, limit int32) ([]*easylabv1.GraphNode, error) {
	res, err := c.Lab.Graph(ctx, connect.NewRequest(&easylabv1.GraphRequest{Org: org, Repo: repo, Limit: limit}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetNodes(), nil
}

// Compare diffs two refs (from/to).
func (c *Client) Compare(ctx context.Context, org, repo, from, to string) ([]*easylabv1.DiffFile, error) {
	res, err := c.Lab.Compare(ctx, connect.NewRequest(&easylabv1.CompareRequest{Org: org, Repo: repo, From: from, To: to}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg.GetFiles(), nil
}

// Rebase reparents a revision onto new parents (snapshot hashes).
func (c *Client) Rebase(ctx context.Context, org, repo, rev string, newParents []string) (string, string, error) {
	res, err := c.Lab.Rebase(ctx, connect.NewRequest(&easylabv1.RebaseRequest{Org: org, Repo: repo, Rev: rev, NewParents: newParents}))
	if err != nil {
		return "", "", errDownstream("easylab", err)
	}
	return res.Msg.GetRevisionId(), res.Msg.GetSnapshot(), nil
}

// Sync pushes a repo snapshot into a service container's dest (default
// /workspace). Returns the file count.
func (c *Client) Sync(ctx context.Context, name, org, repo, rev, dest string, force bool) (int32, error) {
	res, err := c.Ops.Sync(ctx, connect.NewRequest(&easylabv1.SyncRequest{
		Name: name, Org: org, Repo: repo, Rev: rev, Dest: dest, Force: force,
	}))
	if err != nil {
		return 0, errDownstream("easylab", err)
	}
	return res.Msg.GetFiles(), nil
}

// LaunchServiceSpec is the full service launch request (mirrors the proto).
type LaunchServiceSpec struct {
	Name, Image, Kind, Command                string
	Ports                                     []*easylabv1.PortSpec
	Env                                       map[string]string
	Replicas                                  int32
	Group, Network, Namespace, CPUs           string
	MemoryBytes                               uint64
	Annotations                               map[string]string
	Session, Org, Repo                        string
}

// LaunchServiceFull launches a service with the complete spec.
func (c *Client) LaunchServiceFull(ctx context.Context, spec LaunchServiceSpec) (*easylabv1.LaunchServiceResponse, error) {
	res, err := c.Ops.LaunchService(ctx, connect.NewRequest(&easylabv1.LaunchServiceRequest{
		Name: spec.Name, Image: spec.Image, Kind: spec.Kind, Command: spec.Command,
		Ports: spec.Ports, Env: spec.Env, Replicas: spec.Replicas,
		Group: spec.Group, Network: spec.Network, Namespace: spec.Namespace,
		Cpus: spec.CPUs, MemoryBytes: spec.MemoryBytes, Annotations: spec.Annotations,
		Session: spec.Session, Org: spec.Org, Repo: spec.Repo,
	}))
	if err != nil {
		return nil, errDownstream("easylab", err)
	}
	return res.Msg, nil
}
