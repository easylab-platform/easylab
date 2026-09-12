package ci

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/easylab-platform/easylab/internal/k8s"
	publishpkg "github.com/easylab-platform/easylab/internal/publish"
)

// K8sBackend implements Backend on Kubernetes. Everything runs through the
// unified worker-job primitive: CI steps, image builds (a worker job with a
// rootless buildkitd sidecar) and publish-protocol builds. It replaces
// PodmanBackend.
type K8sBackend struct {
	k8s           *k8s.Client
	artifactURL   string
	artifactToken string
	// exportWS materializes org/repo@branch into a directory (the build
	// context). Wired by the host, which owns the easyvcs store.
	exportWS func(org, repo, branch, dir string) error
	// workspaceTar returns the org/repo@branch tree as a tar stream (used to
	// sync a job sandbox's workspace).
	workspaceTar func(org, repo, branch string) ([]byte, error)
}

func NewK8sBackend(kc *k8s.Client) *K8sBackend {
	return &K8sBackend{
		k8s:           kc,
		artifactURL:   os.Getenv("EASYLAB_ARTIFACT_URL"),
		artifactToken: os.Getenv("ARTIFACT_TOKEN"),
	}
}

// SetWorkspaceExporter wires the repo-tree exporter (host callback).
func (b *K8sBackend) SetWorkspaceExporter(f func(org, repo, branch, dir string) error) {
	b.exportWS = f
}

// SetWorkspaceTarball wires the repo-tree tarball exporter (host callback),
// used to seed a CI job sandbox's workspace.
func (b *K8sBackend) SetWorkspaceTarball(f func(org, repo, branch string) ([]byte, error)) {
	b.workspaceTar = f
}

// prepareContext resolves the build context directory for a produce job: an
// explicit Context wins; otherwise a temp dir seeded with the workspace tree.
func (b *K8sBackend) prepareContext(job *Job) string {
	if job.Produce.Context != "" && job.Produce.Context != "." {
		_ = os.MkdirAll(job.Produce.Context, 0o755)
		return job.Produce.Context
	}
	dir := buildTmpDir()
	_ = os.MkdirAll(dir, 0o755)
	if b.exportWS != nil && job.Org != "" && job.Repo != "" {
		_ = b.exportWS(job.Org, job.Repo, job.Branch, dir)
	}
	return dir
}

// Run implements Backend.
func (b *K8sBackend) Run(ctx context.Context, job *Job, rn *Runner, onLog func(string)) (string, error) {
	switch job.Produce.Action {
	case ActionOCIBuild:
		ctxDir := b.prepareContext(job)
		cf := job.Produce.Containerfile
		df := job.Produce.Dockerfile
		if df == "" {
			df = "Dockerfile"
		}
		if cf != "" {
			_ = os.WriteFile(ctxDir+"/"+df, []byte(cf), 0o644)
		}
		if err := b.k8s.BuildImage(ctx, k8s.BuildOptions{
			ContextDir: ctxDir, Dockerfile: df, Image: job.Produce.Tag,
			BuildArgs: mapToArgsMap(job.Produce, job.Steps),
		}, onLog); err != nil {
			return "", err
		}
		return "ok", nil
	case ActionPublishProtocol:
		if _, err := publishpkg.Render(job.Produce.Protocol,
			job.Produce.Name, job.Produce.Version, job.Produce.File,
			b.artifactURL, b.artifactToken); err != nil {
			return "", err
		}
		ctxDir := b.prepareContext(job)
		args := publishBuildArgsMap(job, b.artifactURL, b.artifactToken)
		if err := b.k8s.BuildImage(ctx, k8s.BuildOptions{
			ContextDir: ctxDir, Dockerfile: "Dockerfile",
			Image:     publishTempTag(b.artifactURL, job.Produce.Protocol),
			BuildArgs: args,
		}, onLog); err != nil {
			return "", err
		}
		return "ok", nil
	default:
		return b.runSteps(ctx, job, onLog)
	}
}

// runSteps executes a job's steps in an ephemeral worker sandbox: the
// workspace tree is synced in, each step runs through the worker API, and the
// pod is destroyed on completion. Runtime/image come from the job's runs_on
// labels and container/toolchain.
func (b *K8sBackend) runSteps(ctx context.Context, job *Job, onLog func(string)) (string, error) {
	if len(job.Steps) == 0 {
		return "ok", nil
	}
	if b.k8s == nil {
		return "", fmt.Errorf("k8s backend unavailable")
	}
	profile, err := b.k8s.ResolveRuntime(jobRuntime(job))
	if err != nil {
		return "", err
	}
	image, err := b.jobImage(job, profile)
	if err != nil {
		return "", err
	}
	var tar []byte
	if b.workspaceTar != nil && job.Org != "" && job.Repo != "" {
		tar, err = b.workspaceTar(job.Org, job.Repo, job.Branch)
		if err != nil {
			return "", fmt.Errorf("export workspace: %w", err)
		}
	}

	cmds := make([]k8s.JobCommand, 0, len(job.Steps))
	for _, st := range job.Steps {
		cmds = append(cmds, k8s.JobCommand{
			Name: st.Name, Run: st.Run, Workdir: st.WorkingDirectory, Env: st.Env,
		})
	}

	spec := k8s.JobSpec{
		Name:       "easylab-ci-" + sanitizeName(job.ID) + "-" + fmt.Sprint(time.Now().UnixNano()),
		Image:      image,
		Workspace:  "/workspace",
		Tarball:    tar,
		Commands:   cmds,
		Profile:    &profile,
		WorkerPort: profile.Port(),
	}
	// In-cluster linux jobs use a toolchain image (no built-in worker); inject
	// the worker binary from the node hostPath. VM images ship the worker.
	if !profile.Derived && profile.Image != "" {
		// prebuilt worker image: nothing to inject.
	} else if dir := b.k8s.WorkerHostDir(); dir != "" {
		spec.WorkerBinHostDir = dir
	} else {
		return "", fmt.Errorf("worker binary hostPath unavailable for linux CI job")
	}
	if _, err := b.k8s.RunJob(ctx, spec, "", onLog); err != nil {
		return "", err
	}
	return "ok", nil
}

// jobRuntime maps a job's runs_on labels to a sandbox runtime.
func jobRuntime(job *Job) string {
	for _, l := range job.RunsOn {
		if strings.HasPrefix(l, "os=") {
			return strings.TrimPrefix(l, "os=")
		}
	}
	return ""
}

// jobImage resolves the container image for a job: explicit container, then
// toolchain map, then nil for the profile's own (VM) image.
func (b *K8sBackend) jobImage(job *Job, profile k8s.RuntimeProfile) (string, error) {
	if profile.Image != "" && !profile.Derived {
		return profile.Image, nil
	}
	img := ImageFor(job, nil)
	if img == "" {
		return "", fmt.Errorf("job %s: no container or toolchain image (runs_on=%v)", job.ID, job.RunsOn)
	}
	if b.k8s != nil {
		img = b.k8s.RewriteImageRef(img)
	}
	return img, nil
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 20 {
		out = out[:20]
	}
	if out == "" {
		out = "job"
	}
	return out
}

func mapToArgsMap(p Produce, steps []Step) map[string]string {
	out := map[string]string{}
	if p.Tag != "" {
		out["TAG"] = p.Tag
	}
	for _, st := range steps {
		for k, v := range st.Env {
			out[k] = v
		}
	}
	return out
}

// publishBuildArgsMap mirrors publishBuildArgs but returns a map for BuildImage.
func publishBuildArgsMap(job *Job, artifactURL, artifactToken string) map[string]string {
	out := map[string]string{
		"ARTIFACT_URL":   artifactURL,
		"ARTIFACT_TOKEN": artifactToken,
	}
	p := job.Produce
	if p.Name != "" {
		out["NAME"] = p.Name
	}
	if p.Version != "" {
		out["VERSION"] = p.Version
	}
	if p.File != "" {
		out["FILE"] = p.File
	}
	if p.Protocol == "npm" && artifactURL != "" {
		out["NPMRC_LINE"] = npmrcLine(artifactURL, artifactToken)
	}
	return out
}

// publishTempTag produces a throwaway destination tag for publish-protocol
// builds (the package upload is the deliverable; the image is discarded).
func publishTempTag(artifactURL, protocol string) string {
	host := "easylab"
	if artifactURL != "" {
		host = hostOf(artifactURL)
	}
	return fmt.Sprintf("%s/easylab-publish/%s:%d", host, protocol, os.Getpid())
}

// buildTmpDir returns a dedicated context dir for a produce job when no
// explicit context was provided. It lives under easylab's /data mount so the
// ephemeral build pod's hostPath volume can read it.
func buildTmpDir() string {
	root := filepath.Join(k8s.DataDir(), "tmp")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "."
	}
	dir, err := os.MkdirTemp(root, "easyci-*")
	if err != nil {
		return "."
	}
	_ = os.Chmod(dir, 0o755)
	return dir
}

// ---- helpers ----

func hostOf(ref string) string {
	ref = strings.TrimPrefix(ref, "https://")
	ref = strings.TrimPrefix(ref, "http://")
	if i := strings.Index(ref, "/"); i > 0 {
		return ref[:i]
	}
	return ref
}

// npmrcLine builds the .npmrc auth line npm needs (registry URL sans scheme,
// INCLUDING the /pkgs/npm path; a host-only key never matches).
func npmrcLine(artifactURL, token string) string {
	host := strings.TrimPrefix(strings.TrimPrefix(artifactURL, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	if strings.HasPrefix(artifactURL, "https://") {
		host = strings.TrimSuffix(host, ":443")
	} else {
		host = strings.TrimSuffix(host, ":80")
	}
	return "//" + host + "/pkgs/npm/:_authToken=" + token
}
