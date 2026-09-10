package ci

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// Backend is the execution engine seam. A backend can lift a container image
// or a host runner to run a job. The concrete implementations are:
//   - PodmanBackend (oci-build / container runtime on the podman sidecar)
//   - EasyworkerBackend (dispatch a job to a registered host easyworker)
type Backend interface {
	// Run executes one job's steps (and the produce action) against the
	// resolved runner, streaming log lines to onLog. Returns the job result.
	Run(ctx context.Context, job *Job, rn *Runner, onLog func(line string)) (string, error)
}

// ResolveRunner picks the runner for a job: explicit container wins over a
// toolchain label-mapped image; otherwise match by runs-on labels.
func ResolveRunner(reg *RunnerRegistry, job *Job) (*Runner, error) {
	rn := reg.Match(job.RunsOn)
	if rn == nil {
		return nil, fmt.Errorf("no runner matches %v", job.RunsOn)
	}
	return rn, nil
}

// ImageFor resolves the base image for a job, honoring: explicit container,
// then toolchain default map, then the runner's own toolchain map.
func ImageFor(job *Job, rn *Runner) string {
	if job.Container != "" {
		return job.Container
	}
	for _, l := range job.RunsOn {
		if tc := labelValue(l, "toolchain"); tc != "" {
			if rn != nil && rn.Toolchains != nil {
				if img, ok := rn.Toolchains[tc]; ok {
					return img
				}
			}
			if img, ok := Toolchains[tc]; ok {
				return img
			}
		}
	}
	return ""
}

// labelValue extracts key=value from a label, "" if absent.
func labelValue(label, key string) string {
	for i := 0; i < len(label); i++ {
		if label[i] == '=' {
			if label[:i] == key {
				return label[i+1:]
			}
			return ""
		}
	}
	return ""
}

// Scheduler runs the job DAG of a Run in needs order (topological). On any
// job failure the run fails (first release: no continue-on-error).
type Scheduler struct {
	reg   *RunnerRegistry
	back  Backend
	onLog func(jobID, line string)
}

func NewScheduler(reg *RunnerRegistry, back Backend, onLog func(jobID, line string)) *Scheduler {
	return &Scheduler{reg: reg, back: back, onLog: onLog}
}

// Schedule materializes and runs jobs of a workflow synchronously (kept for
// tests and any caller that wants the terminal state).
func (s *Scheduler) Schedule(ctx context.Context, wf *Workflow) (run *Run, err error) {
	run = s.NewRun(wf)
	return run, s.Run(ctx, wf, run)
}

// NewRun materializes the job instances for a workflow and returns a run in
// StatePending WITHOUT executing anything. Callers that run asynchronously
// store the run first (so GetRun sees it as pending) and then call Run.
func (s *Scheduler) NewRun(wf *Workflow) *Run {
	run := &Run{ID: "run-" + newID(), WorkflowID: wf.ID, State: StatePending, StartedAt: time.Now()}
	for _, j := range wf.Jobs {
		run.Jobs = append(run.Jobs, JobInstance{JobID: newID(), DefID: j.ID, State: StatePending})
	}
	return run
}

// Run executes a materialized run (see NewRun) to completion, recording the
// terminal state on the run. Safe to call in a goroutine.
func (s *Scheduler) Run(ctx context.Context, wf *Workflow, run *Run) error {
	run.SetState(StateRunning)
	if err := s.walk(ctx, wf, run); err != nil {
		run.SetState(StateFailure)
		run.mu.Lock()
		run.FinishedAt = time.Now()
		run.mu.Unlock()
		return err
	}
	run.SetState(StateSuccess)
	run.mu.Lock()
	run.FinishedAt = time.Now()
	run.mu.Unlock()
	return nil
}

// walk executes jobs in topo order following needs.
func (s *Scheduler) walk(ctx context.Context, wf *Workflow, run *Run) error {
	byDef := map[string]*Job{}
	for i := range wf.Jobs {
		byDef[wf.Jobs[i].ID] = &wf.Jobs[i]
	}
	done := map[string]bool{}
	// simple repeated passes over ready jobs (needs all done)
	for len(done) < len(wf.Jobs) {
		progress := false
		for _, jv := range wf.Jobs {
			j := byDef[jv.ID]
			if done[j.ID] {
				continue
			}
			if !allDone(j.Needs, done) {
				continue
			}
			// find instance
			var inst JobInstance
			for _, inst = range run.Jobs {
				if inst.DefID == j.ID {
					break
				}
			}
			j.Org, j.Repo, j.Branch = wf.Org, wf.Repo, wf.Branch
			run.SetJobState(j.ID, StateRunning, "")
			res, rerr := s.back.Run(ctx, j, s.pickRunner(j), func(line string) {
				if s.onLog != nil {
					s.onLog(inst.JobID, line)
				}
			})
			if rerr != nil || res != "ok" {
				result := res
				if rerr != nil {
					result = rerr.Error()
				}
				run.SetJobState(j.ID, StateFailure, result)
				return fmt.Errorf("job %s failed: %s", j.ID, result)
			}
			run.SetJobState(j.ID, StateSuccess, "")
			done[j.ID] = true
			progress = true
		}
		if !progress {
			// cycle / unsatisfiable needs
			return fmt.Errorf("workflow jobs unresolved (cycle or missing needs)")
		}
	}
	return nil
}

func (s *Scheduler) pickRunner(j *Job) *Runner {
	rn, _ := ResolveRunner(s.reg, j)
	return rn
}

func allDone(needs []string, done map[string]bool) bool {
	for _, n := range needs {
		if !done[n] {
			return false
		}
	}
	return true
}

func newID() string {
	var b [12]byte
	n, err := rand.Read(b[:])
	if err != nil || n != len(b) {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
