// Package ci implements the event-driven CI/Workflow layer: declarative
// workflows (on -> jobs) instantiated as Runs, job DAG scheduling (needs),
// a runner registry (labels + heartbeat) and the produce actions
// (oci-build / artifact-upload / publish-protocol). CI is independent of the
// dev (sandbox/service) world: runners with session_bound=true do not join
// the CI pool.
package ci

import (
	"sync"
	"time"
)

// Action describes one produce action (the "outcome" semantics; no kind).
type Action string

const (
	ActionOCIBuild        Action = "oci-build"
	ActionArtifactUpload  Action = "artifact-upload"
	ActionPublishProtocol Action = "publish-protocol"
)

// Toolchains is the built-in toolchain-label -> base image map. Explicit
// `container` overrides these; a runner's own labels override the scheduler.
var Toolchains = map[string]string{
	"go":     "docker.io/library/golang:1.26-alpine",
	"rust":   "docker.io/library/rust:1-alpine",
	"node":   "docker.io/library/node:22-alpine",
	"python": "docker.io/library/python:3.12-alpine",
	"ruby":   "docker.io/library/ruby:3.3-alpine",
	"dotnet": "docker.io/library/dotnet:8.0-sdk",
	"swift":  "docker.io/library/swift:6.1",
	"xcode":  "", // preinstalled on mac host runners (no container)
}

// ToolchainImage resolves a toolchain label to its default image.
func ToolchainImage(tc string) string { return Toolchains[tc] }

// Job is a single CI job definition (a DAG node). It is declarative and
// stateless — a Run materialises one JobInstance per Job.
type Job struct {
	ID     string
	Needs  []string
	RunsOn []string // runner labels: os=, arch=, is_container=, toolchain=
	// Workspace coords (set by the scheduler from the owning Workflow) so
	// produce backends can export the repo tree as the build context.
	Org              string
	Repo             string
	Branch           string
	Container        string // explicit image override
	WorkingDirectory string
	Steps            []Step
	Produce          Produce
}

// Step is one command in a job.
type Step struct {
	Name             string
	Run              string
	Env              map[string]string
	WorkingDirectory string
}

// Produce declares the platform-managed outcome action.
type Produce struct {
	Action        Action
	Context       string
	Dockerfile    string
	Tag           string
	Path          string
	Destination   string
	Ref           string
	Protocol      string
	Name          string
	Version       string
	File          string
	Containerfile string
}

// Workflow is the declarative event-driven automation unit.
type Workflow struct {
	ID     string
	Name   string
	Org    string
	Repo   string
	Branch string
	On     []string // push | manual
	Jobs   []Job
}

// Runner is a registered execution entity with labels.
type Runner struct {
	ID           string
	Labels       []string
	Backend      string // podman | host
	Addr         string
	LastSeen     time.Time
	SessionBound bool
	// Capability resolves the base image for a job's toolchain (nil = use
	// Toolcharts map).
	Toolchains map[string]string
}

// State is the running status of a job/run.
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateSuccess   State = "success"
	StateFailure   State = "failure"
	StateCancelled State = "cancelled"
)

// JobInstance is one job's materialized execution in a Run.
type JobInstance struct {
	JobID  string
	DefID  string
	State  State
	Result string
}

// Run is one instantiation of a Workflow. It is mutated by the scheduler
// goroutine while readers (GetRun/ListRuns) snapshot it, so all state changes
// go through the methods below and every read takes Snapshot().
type Run struct {
	mu         sync.RWMutex
	ID         string
	WorkflowID string
	State      State
	Jobs       []JobInstance
	StartedAt  time.Time
	FinishedAt time.Time
}

// Snapshot returns a deep copy safe for concurrent reads.
func (r *Run) Snapshot() *Run {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cp := &Run{
		ID: r.ID, WorkflowID: r.WorkflowID, State: r.State,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
		Jobs: make([]JobInstance, len(r.Jobs)),
	}
	copy(cp.Jobs, r.Jobs)
	return cp
}

// SetState updates the run state.
func (r *Run) SetState(s State) {
	r.mu.Lock()
	r.State = s
	r.mu.Unlock()
}

// SetJobState updates one job instance by def id.
func (r *Run) SetJobState(defID string, s State, result string) {
	r.mu.Lock()
	for i := range r.Jobs {
		if r.Jobs[i].DefID == defID {
			r.Jobs[i].State = s
			if result != "" {
				r.Jobs[i].Result = result
			}
			break
		}
	}
	r.mu.Unlock()
}
