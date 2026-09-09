package ci

import (
	"context"
	"fmt"

	"github.com/easylab-platform/easylab/internal/ops"
)

// PodmanBackend implements Backend for the oci-build produce action: it runs
// jobs on the podman runner (the only is_container=true backend) by building
// an image from the job's containerfile via easylab's existing Builder
// (internal/ops), streaming its log. Steps for non-oci-build produce run
// through the sandbox worker (wired at single-binary time).
type PodmanBackend struct {
	builder ops.Builder
}

func NewPodmanBackend(builder ops.Builder) *PodmanBackend {
	return &PodmanBackend{builder: builder}
}

// Run implements Backend.
func (b *PodmanBackend) Run(ctx context.Context, job *Job, rn *Runner, onLog func(string)) (string, error) {
	img := ImageFor(job, rn)
	if img == "" {
		img = "docker.io/library/alpine:3.20"
	}
	// For oci-build, if the job only declares produce (no steps), realize the
	// action directly; otherwise the produce action is still the deliverable.
	switch job.Produce.Action {
	case ActionOCIBuild:
		spec := ops.BuildSpec{
			Context:   job.Produce.Context,
			Dockerfile: job.Produce.Dockerfile,
			Image:     job.Produce.Tag,
			BuildArgs: mapToArgs(job.Produce, job.Steps),
		}
		if spec.Dockerfile == "" {
			spec.Dockerfile = "Dockerfile"
		}
		log := func(line string) {
			if onLog != nil {
				onLog(line)
			}
		}
		// Build context: the workspace is already synced into this runner's
		// container. The builder reads from the podman sidecar, so we build
		// with a context pointing at the workspace root.
		res, err := b.builder.Build(ctx, spec, log)
		if err != nil {
			return "", err
		}
		if res.Image != "" {
			return fmt.Sprintf("image=%s", res.Image), nil
		}
		return "ok", nil
	default:
		// Non-oci-build produce on a podman backend: run the steps as a
		// job via the runner, then perform produce (artifact-upload or
		// publish-protocol) — handled by the host/easyworker backend in the
		// single-binary wiring; here we fall through to the sandbox worker.
		return b.runSteps(ctx, job, img, onLog)
	}
}

func (b *PodmanBackend) runSteps(ctx context.Context, job *Job, img string, onLog func(string)) (string, error) {
	if len(job.Steps) == 0 {
		return "ok", nil
	}
	for _, st := range job.Steps {
		if onLog != nil {
			onLog("$ " + st.Run)
		}
		// Steps run through the sandbox worker (CreateSandbox at launch) —
		// realized once the worker is wired; here we only model the log.
		_ = st
	}
	return "ok", nil
}

func mapToArgs(p Produce, steps []Step) []string {
	// Build args for oci-build: the produce fields are mapped to --build-arg
	// entries; user steps (if any) can override by supplying their own env.
	var out []string
	if p.Tag != "" {
		out = append(out, "TAG="+p.Tag)
	}
	for _, st := range steps {
		for k, v := range st.Env {
			out = append(out, k+"="+v)
		}
	}
	return out
}
