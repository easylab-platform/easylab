package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easylab-proto/easylab/v1/easylabv1connect"
	"github.com/easylab-platform/easylab/internal/ci"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// connWorkflow implements easylabv1connect.WorkflowServiceHandler: declarative
// workflows (on -> jobs), run DAG scheduling, runner registry, and produce
// actions (oci-build / artifact-upload / publish-protocol). CI is independent
// of the dev (sandbox/service) world: runners with session_bound=true are not
// part of this pool.
type connWorkflow struct {
	s       *server
	reg     *ci.RunnerRegistry
	sch     *ci.Scheduler
	logsMu  sync.Mutex
	jobLogs map[string][]string // job id -> tail of build output
}

var _ easylabv1connect.WorkflowServiceHandler = (*connWorkflow)(nil)

// NewWorkflowService wires the CI scheduler: podman backend for oci-build; a
// registry for host runners; sandbox worker backend for steps.
func NewWorkflowService(s *server) *connWorkflow {
	reg := ci.NewRunnerRegistry()
	// The builder may be nil in tests (bare &server{cs:cs}); the scheduler
	// still works for registry/runner/zoom when no backend produces jobs.
	back := ci.NewPodmanBackend(nil)
	if s.ops != nil && s.ops.builder != nil {
		back = ci.NewPodmanBackend(s.ops.builder)
	}
	// Seed produce build contexts with the repo tree at the branch head.
	back.SetWorkspaceExporter(func(org, repo, branch, dir string) error {
		return exportRepoTree(s, org, repo, branch, dir)
	})
	c := &connWorkflow{s: s, reg: reg, jobLogs: map[string][]string{}}
	c.sch = ci.NewScheduler(reg, back, func(jobID, line string) {
		c.logsMu.Lock()
		defer c.logsMu.Unlock()
		logs := append(c.jobLogs[jobID], line)
		if len(logs) > 400 {
			logs = logs[len(logs)-400:]
		}
		c.jobLogs[jobID] = logs
	})
	return c
}

func protoWorkflow(w *ci.Workflow) *easylabv1.Workflow {
	jobs := make([]*easylabv1.JobDef, 0, len(w.Jobs))
	for _, j := range w.Jobs {
		steps := make([]*easylabv1.Step, 0, len(j.Steps))
		for _, st := range j.Steps {
			steps = append(steps, &easylabv1.Step{
				Name: st.Name, Run: st.Run, Env: st.Env, WorkingDirectory: st.WorkingDirectory,
			})
		}
		jobs = append(jobs, &easylabv1.JobDef{
			Id: j.ID, Needs: j.Needs, RunsOn: j.RunsOn, Container: j.Container,
			WorkingDirectory: j.WorkingDirectory, Steps: steps,
			Produce: protoProduce(j.Produce),
		})
	}
	return &easylabv1.Workflow{
		Id: w.ID, Name: w.Name, Org: w.Org, Repo: w.Repo, Branch: w.Branch,
		On: &easylabv1.Trigger{Events: w.On}, Jobs: jobs,
	}
}

func protoProduce(p ci.Produce) *easylabv1.Produce {
	return &easylabv1.Produce{
		Action: string(p.Action), Context: p.Context, Dockerfile: p.Dockerfile,
		Tag: p.Tag, Path: p.Path, Destination: p.Destination, Ref: p.Ref,
		Protocol: p.Protocol, Name: p.Name, Version: p.Version, File: p.File,
		Containerfile: p.Containerfile,
	}
}

func (c *connWorkflow) CreateWorkflow(ctx context.Context, req *connect.Request[easylabv1.CreateWorkflowRequest]) (*connect.Response[easylabv1.CreateWorkflowResponse], error) {
	w := fromProtoWorkflow(req.Msg.GetWorkflow())
	if w.ID == "" {
		w.ID = "wf-" + newID()
	}
	c.s.workflows.Store(w.ID, w)
	return connect.NewResponse(&easylabv1.CreateWorkflowResponse{Workflow: protoWorkflow(w)}), nil
}

func (c *connWorkflow) GetWorkflow(ctx context.Context, req *connect.Request[easylabv1.GetWorkflowRequest]) (*connect.Response[easylabv1.GetWorkflowResponse], error) {
	v, ok := c.s.workflows.Load(req.Msg.Id)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, connect.NewError(connect.CodeNotFound, errWorkflowNotFound()))
	}
	return connect.NewResponse(&easylabv1.GetWorkflowResponse{Workflow: protoWorkflow(v.(*ci.Workflow))}), nil
}

func (c *connWorkflow) ListWorkflows(ctx context.Context, req *connect.Request[easylabv1.ListWorkflowsRequest]) (*connect.Response[easylabv1.ListWorkflowsResponse], error) {
	var ws []*easylabv1.Workflow
	c.s.workflows.Range(func(_, v interface{}) bool {
		w := v.(*ci.Workflow)
		if (req.Msg.Org == "" || w.Org == req.Msg.Org) && (req.Msg.Repo == "" || w.Repo == req.Msg.Repo) {
			ws = append(ws, protoWorkflow(w))
		}
		return true
	})
	return connect.NewResponse(&easylabv1.ListWorkflowsResponse{Workflows: ws}), nil
}

func (c *connWorkflow) TriggerRun(ctx context.Context, req *connect.Request[easylabv1.TriggerRunRequest]) (*connect.Response[easylabv1.TriggerRunResponse], error) {
	v, ok := c.s.workflows.Load(req.Msg.WorkflowId)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, connect.NewError(connect.CodeNotFound, errWorkflowNotFound()))
	}
	w := v.(*ci.Workflow)
	run, err := c.sch.Schedule(context.Background(), w)
	if err != nil {
		log.Printf("workflow %s run failed: %v", w.ID, err)
	}
	c.s.runs.Store(run.ID, run)
	return connect.NewResponse(&easylabv1.TriggerRunResponse{Run: protoRun(run)}), nil
}

func (c *connWorkflow) GetRun(ctx context.Context, req *connect.Request[easylabv1.GetRunRequest]) (*connect.Response[easylabv1.GetRunResponse], error) {
	v, ok := c.s.runs.Load(req.Msg.Id)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, connect.NewError(connect.CodeNotFound, errRunNotFound()))
	}
	return connect.NewResponse(&easylabv1.GetRunResponse{Run: protoRun(v.(*ci.Run))}), nil
}

func (c *connWorkflow) ListRuns(ctx context.Context, req *connect.Request[easylabv1.ListRunsRequest]) (*connect.Response[easylabv1.ListRunsResponse], error) {
	var runs []*easylabv1.Run
	c.s.runs.Range(func(_, v interface{}) bool {
		r := v.(*ci.Run)
		if req.Msg.WorkflowId == "" || r.WorkflowID == req.Msg.WorkflowId {
			runs = append(runs, protoRun(r))
		}
		return true
	})
	return connect.NewResponse(&easylabv1.ListRunsResponse{Runs: runs}), nil
}

// RunJobLog streams a job's log for a run (job id within the run).
func (c *connWorkflow) RunJobLog(ctx context.Context, req *connect.Request[easylabv1.RunJobLogRequest], stream *connect.ServerStream[easylabv1.RunJobLogResponse]) error {
	c.logsMu.Lock()
	logs := append([]string(nil), c.jobLogs[req.Msg.JobId]...)
	c.logsMu.Unlock()
	if len(logs) == 0 {
		return stream.Send(&easylabv1.RunJobLogResponse{Stream: "state", Line: "run " + req.Msg.RunId + " job " + req.Msg.JobId + " complete"})
	}
	for _, line := range logs {
		if err := stream.Send(&easylabv1.RunJobLogResponse{Stream: "build", Line: line}); err != nil {
			return err
		}
	}
	return nil
}

func (c *connWorkflow) CancelRun(ctx context.Context, req *connect.Request[easylabv1.CancelRunRequest]) (*connect.Response[easylabv1.CancelRunResponse], error) {
	v, ok := c.s.runs.Load(req.Msg.Id)
	if !ok {
		return connect.NewResponse(&easylabv1.CancelRunResponse{Ok: false}), nil
	}
	r := v.(*ci.Run)
	r.State = ci.StateCancelled
	c.s.runs.Store(r.ID, r)
	return connect.NewResponse(&easylabv1.CancelRunResponse{Ok: true}), nil
}

func (c *connWorkflow) RegisterRunner(ctx context.Context, req *connect.Request[easylabv1.RegisterRunnerRequest]) (*connect.Response[easylabv1.RegisterRunnerResponse], error) {
	rn := req.Msg.GetRunner()
	if rn == nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, connect.NewError(connect.CodeInvalidArgument, errRunnerRequired()))
	}
	r := &ci.Runner{
		ID: rn.Id, Labels: rn.Labels, Backend: rn.Backend, Addr: rn.Addr,
		SessionBound: rn.SessionBound,
	}
	if err := c.reg.Register(r); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&easylabv1.RegisterRunnerResponse{Ok: true}), nil
}

func (c *connWorkflow) ListRunners(ctx context.Context, req *connect.Request[easylabv1.ListRunnersRequest]) (*connect.Response[easylabv1.ListRunnersResponse], error) {
	var out []*easylabv1.Runner
	for _, rn := range c.reg.List() {
		out = append(out, &easylabv1.Runner{
			Id: rn.ID, Labels: rn.Labels, Backend: rn.Backend, Addr: rn.Addr,
			LastSeen: rn.LastSeen.UnixMilli(), SessionBound: rn.SessionBound,
		})
	}
	return connect.NewResponse(&easylabv1.ListRunnersResponse{Runners: out}), nil
}

func fromProtoWorkflow(wf *easylabv1.Workflow) *ci.Workflow {
	if wf == nil {
		return &ci.Workflow{}
	}
	out := &ci.Workflow{
		ID: wf.Id, Name: wf.Name, Org: wf.Org, Repo: wf.Repo, Branch: wf.Branch,
	}
	if wf.On != nil {
		out.On = wf.On.Events
	}
	for _, j := range wf.Jobs {
		if j == nil {
			continue
		}
		steps := make([]ci.Step, 0, len(j.Steps))
		for _, st := range j.Steps {
			if st == nil {
				continue
			}
			steps = append(steps, ci.Step{Name: st.Name, Run: st.Run, Env: st.Env, WorkingDirectory: st.WorkingDirectory})
		}
		out.Jobs = append(out.Jobs, ci.Job{
			ID: j.Id, Needs: j.Needs, RunsOn: j.RunsOn, Container: j.Container,
			WorkingDirectory: j.WorkingDirectory, Steps: steps,
			Produce: ci.Produce{
				Action: ci.Action(j.Produce.GetAction()), Context: j.Produce.GetContext(),
				Dockerfile: j.Produce.GetDockerfile(), Tag: j.Produce.GetTag(),
				Path: j.Produce.GetPath(), Destination: j.Produce.GetDestination(),
				Ref: j.Produce.GetRef(), Protocol: j.Produce.GetProtocol(),
				Name: j.Produce.GetName(), Version: j.Produce.GetVersion(), File: j.Produce.GetFile(),
				Containerfile: j.Produce.GetContainerfile(),
			},
		})
	}
	return out
}

func protoRun(r *ci.Run) *easylabv1.Run {
	if r == nil {
		return &easylabv1.Run{}
	}
	jobs := make([]*easylabv1.JobInstance, 0, len(r.Jobs))
	for _, j := range r.Jobs {
		jobs = append(jobs, &easylabv1.JobInstance{Id: j.JobID, DefId: j.DefID, Status: string(j.State), Result: j.Result})
	}
	started := ""
	if !r.StartedAt.IsZero() {
		started = r.StartedAt.UTC().Format(time.RFC3339)
	}
	finished := ""
	if !r.FinishedAt.IsZero() {
		finished = r.FinishedAt.UTC().Format(time.RFC3339)
	}
	return &easylabv1.Run{
		Id: r.ID, WorkflowId: r.WorkflowID, Status: string(r.State), Jobs: jobs,
		StartedAt: started, FinishedAt: finished,
	}
}

// errors as plain connect errors
func errWorkflowNotFound() error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("workflow not found"))
}
func errRunNotFound() error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("run not found"))
}
func errRunnerRequired() error { return fmt.Errorf("runner required") }

// newID short unique id (used to key workflows/runs).
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// exportRepoTree materializes org/repo@branch into dir (the repo tree as the
// produce build context), reusing the sandbox-sync tar builder.
func exportRepoTree(s *server, org, repoName, branch, dir string) error {
	r, err := s.cs.OpenRepo(store.RepoRef{Namespace: org, Name: repoName})
	if err != nil {
		return fmt.Errorf("open %s/%s: %w", org, repoName, err)
	}
	ws := revision.NewWorkspace(r)
	treeID, err := treeOfRef(ws, r, branch)
	if err != nil {
		return fmt.Errorf("ref %s: %w", branch, err)
	}
	tarball, _, err := buildTreeTar(ws, treeID)
	if err != nil {
		return err
	}
	return extractTarTo(dir, tarball)
}

// extractTarTo unpacks a tarball into dir (regular files + directories).
func extractTarTo(dir string, tarball []byte) error {
	tr := tar.NewReader(bytes.NewReader(tarball))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		clean := filepath.Clean("/" + hdr.Name)
		if clean == "/" || strings.Contains(clean, "..") {
			continue
		}
		target := filepath.Join(dir, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0o777|0o400)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
}
