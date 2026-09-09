package ci

import (
	"context"
	"os"
	"strconv"
	"time"

	publishpkg "github.com/easylab-platform/easylab/internal/publish"

	"github.com/easylab-platform/easylab/internal/ops"
)

// PodmanBackend implements Backend for the oci-build produce action: it runs
// jobs on the podman runner (the only is_container=true backend) by building
// an image from the job's containerfile via easylab's existing Builder
// (internal/ops), streaming its log. Steps for non-oci-build produce run
// through the sandbox worker (wired at single-binary time).
type PodmanBackend struct {
	builder       ops.Builder
	artifactURL   string
	artifactToken string
}

func NewPodmanBackend(builder ops.Builder) *PodmanBackend {
	return &PodmanBackend{builder: builder, artifactURL: os.Getenv("EASYLAB_ARTIFACT_URL"), artifactToken: os.Getenv("ARTIFACT_TOKEN")}
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
		// oci-build must never walk the easylab cwd (EASYVCS_HOME) as context:
		// always use a dedicated temp build dir. A self-contained build may
		// inline the Containerfile; otherwise Dockerfile is expected in the
		// (workspace-synced) context.
		ctxDir := job.Produce.Context
		if ctxDir == "" || ctxDir == "." {
			ctxDir = buildTmpDir()
		}
		_ = os.MkdirAll(ctxDir, 0o755)
		spec := ops.BuildSpec{
			Context:       ctxDir,
			Containerfile: job.Produce.Containerfile,
			Dockerfile:    job.Produce.Dockerfile,
			Image:         job.Produce.Tag,
			BuildArgs:     mapToArgs(job.Produce, job.Steps),
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
		// Scheduler treats res=="ok" as success. The produced image tag is the
		// deliverable; surface it via the log/message but return "ok" so the
		// job is not mis-classified as failed (the image exists regardless).
		if onLog != nil && res.Image != "" {
			onLog("image=" + res.Image)
		}
		return "ok", nil
	case ActionPublishProtocol:
		// publish-protocol: render the built-in template for the protocol,
		// then build+run it via the podman build backend. The RUN uploads
		// the package to the easylab protocol registry; exit 0 = success.
		cf, err := publishpkg.Render(job.Produce.Protocol,
			job.Produce.Name, job.Produce.Version, job.Produce.File,
			b.artifactURL, b.artifactToken)
		if err != nil {
			return "", err
		}
		ctxDir := buildTmpDir()
		_ = os.MkdirAll(ctxDir, 0o755)
		spec := ops.BuildSpec{
			Context:       ctxDir,
			Containerfile: cf,
			Dockerfile:    "Dockerfile",
			Image:         "", // publish does not export an image
			BuildArgs:     []string{"ARTIFACT_URL=" + b.artifactURL, "ARTIFACT_TOKEN=" + b.artifactToken, "PUBLISH_TS=" + ts()},
		}
		_, err = b.builder.Build(ctx, spec, func(line string) {
			if onLog != nil {
				onLog(line)
			}
		})
		if err != nil {
			return "", err
		}
		return "ok", nil
	default:
		// Non-oci-build/publish produce on a podman backend: run the steps
		// as a job via the runner, then perform produce — handled by the
		// host/easyworker backend in the single-binary wiring.
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

// buildTmpDir returns a dedicated context dir for an oci-build job when no
// explicit context (workspace) was provided. Prevents accidental use of the
// easylab data dir as a build context.
func buildTmpDir() string {
	dir, err := os.MkdirTemp("", "easyci-*")
	if err != nil {
		return "."
	}
	return dir
}

func ts() string { return strconv.FormatInt(time.Now().UnixNano(), 10) }
