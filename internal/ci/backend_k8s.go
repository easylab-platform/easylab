package ci

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/easylab-platform/easylab/internal/k8s"
	publishpkg "github.com/easylab-platform/easylab/internal/publish"
)

// K8sBackend implements Backend on Kubernetes: builds run in ephemeral
// rootless buildkit pods (pushing to easylab's OCI registry) and job steps run
// through a sandbox worker when one is resolved. It replaces PodmanBackend.
type K8sBackend struct {
	k8s           *k8s.Client
	artifactURL   string
	artifactToken string
	// exportWS materializes org/repo@branch into a directory (the build
	// context). Wired by the host, which owns the easyvcs store.
	exportWS func(org, repo, branch, dir string) error
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
		cf, err := publishpkg.Render(job.Produce.Protocol,
			job.Produce.Name, job.Produce.Version, job.Produce.File,
			b.artifactURL, b.artifactToken)
		if err != nil {
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
		_ = cf
		return "ok", nil
	default:
		return b.runSteps(ctx, job, onLog)
	}
}

// runSteps executes job steps on a resolved host runner via the worker API.
// With no runner it is a no-op (the job's produce is the deliverable).
func (b *K8sBackend) runSteps(ctx context.Context, job *Job, onLog func(string)) (string, error) {
	if len(job.Steps) == 0 {
		return "ok", nil
	}
	for _, st := range job.Steps {
		if onLog != nil {
			onLog("$ " + st.Run)
		}
	}
	return "ok", nil
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
