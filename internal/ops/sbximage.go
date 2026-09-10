package ops

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/go-connections/nat"
)

// WorkerBinPath returns the easyworker binary's local path inside the easylab
// image (override with EASYLAB_WORKER_BIN). It is injected into sandbox base
// images as the ENTRYPOINT via a cached derived image.
func WorkerBinPath() string {
	if v := os.Getenv("EASYLAB_WORKER_BIN"); v != "" {
		return v
	}
	return "/usr/local/lib/easyworker/easyworker-linux-amd64"
}

// WorkerRef identifies the easyworker build fetched from the artifact registry.
// EASYLAB_WORKERREF is "name@version" (default easyworker@v0.1.0); the binary
// gets the same canonical filename on every platform.
const (
	defaultWorkerName    = "easyworker"
	defaultWorkerVersion = "v0.4.1"
	workerBinFilename    = "easyworker-linux-amd64"
)

func WorkerRef() (name, version string) {
	ref := os.Getenv("EASYLAB_WORKER_REF")
	if ref == "" {
		return defaultWorkerName, defaultWorkerVersion
	}
	if i := stringsIndexByte(ref, '@'); i > 0 {
		return ref[:i], ref[i+1:]
	}
	return ref, defaultWorkerVersion
}

// loadWorkerBin resolves the easyworker binary. Precedence:
//  1. EASYLAB_WORKER_BIN (explicit local file)
//  2. the on-image copy (present in the deployed easylab image)
//  3. `registry` — fetch from the artifact generic store (EASYLAB_ARTIFACT_URL)
//
// The registry path lets a build host with neither the easyworker source tree
// nor the binary on disk still assemble sandbox images.
func loadWorkerBin(ctx context.Context) ([]byte, error) {
	if v := os.Getenv("EASYLAB_WORKER_BIN"); v != "" {
		return os.ReadFile(v)
	}
	if b, err := os.ReadFile(WorkerBinPath()); err == nil {
		return b, nil
	}
	return fetchWorkerBin(ctx)
}

// fetchWorkerBin downloads <artifactURL>/pkgs/generic/<name>/<version>/<file>.
func fetchWorkerBin(ctx context.Context) ([]byte, error) {
	base := os.Getenv("EASYLAB_ARTIFACT_URL")
	if base == "" {
		return nil, fmt.Errorf("easyworker binary not found and EASYLAB_ARTIFACT_URL unset")
	}
	name, version := WorkerRef()
	url := fmt.Sprintf("%s/pkgs/generic/%s/%s/%s", trimSlash(base), name, version, workerBinFilename)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if tok := os.Getenv("ARTIFACT_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func stringsIndexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// SandboxTag derives the deterministic derived-image tag for a base image:
// sha256(base ref + worker binary contents)[:16]. A worker version bump
// changes the binary hash and therefore the tag — sandboxes rebuild exactly
// once per (base, worker) pair and hit the local image cache afterwards.
func SandboxTag(baseImage string, workerBin []byte) string {
	h := sha256.New()
	h.Write([]byte(baseImage))
	h.Write([]byte{'|'})
	h.Write(workerBin)
	// The registry-like domain keeps the runner's short-name qualifier from
	// rewriting the local-only tag into an internal-registry pull.
	return "sbx.internal/easyworker:" + hex.EncodeToString(h.Sum(nil))[:16]
}

// EnsureSandboxImage makes sure a derived image (base + easyworker
// ENTRYPOINT) exists in the local podman store, building it when missing.
// Returns (tag, built). The Containerfile avoids RUN so shell-less bases
// (scratch/distroless) build too; WORKDIR creates the workspace without a
// shell.
func (r *PodmanServiceRunner) EnsureSandboxImage(ctx context.Context, baseImage, workspace string) (string, bool, error) {
	bin, err := loadWorkerBin(ctx)
	if err != nil {
		return "", false, fmt.Errorf("worker binary: %w", err)
	}
	tag := SandboxTag(baseImage, bin)

	// Cache hit?
	if _, _, err := r.cli.ImageInspectWithRaw(ctx, tag); err == nil {
		return tag, false, nil
	}

	containerfile := fmt.Sprintf(`FROM %s
COPY easyworker /usr/local/bin/easyworker
ENV WORKER_PORT=48080
WORKDIR %s
ENTRYPOINT ["/usr/local/bin/easyworker"]
`, baseImage, workspace)

	ctxTar, err := buildSandboxContext(bin, containerfile)
	if err != nil {
		return "", false, err
	}
	resp, err := r.cli.ImageBuild(ctx, ctxTar, build.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: "Containerfile",
		Remove:     true,
	})
	if err != nil {
		return "", false, fmt.Errorf("derive build: %w", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body) // drain for buildah; errors surface below
	if _, _, err := r.cli.ImageInspectWithRaw(ctx, tag); err != nil {
		return "", false, fmt.Errorf("derive build did not produce %s: %s", tag, tail(out, 400))
	}
	return tag, true, nil
}

// buildSandboxContext packs the worker binary (mode 0755) + Containerfile.
func buildSandboxContext(bin []byte, containerfile string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: "easyworker", Mode: 0o755, Size: int64(len(bin)), ModTime: time.Now()}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	if _, err := tw.Write(bin); err != nil {
		return nil, err
	}
	cf := []byte(containerfile)
	hdr = &tar.Header{Name: "Containerfile", Mode: 0o644, Size: int64(len(cf)), ModTime: time.Now()}
	if err := tw.WriteHeader(hdr); err != nil {
		return nil, err
	}
	if _, err := tw.Write(cf); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return bytes.NewReader(buf.Bytes()), nil
}

func tail(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[len(b)-n:])
}

// WorkerAddr resolves the loopback address of a container's published port
// (auto-assigned host port read back from inspect). This is the podman
// flavor's reachability model: sandbox containers live on host/internal
// networks without routable IPs, so easylab talks to workers via
// 127.0.0.1:<published-port>.
func (r *PodmanServiceRunner) WorkerAddr(ctx context.Context, name string, containerPort int) (string, error) {
	insp, err := r.cli.ContainerInspect(ctx, name)
	if err != nil {
		return "", err
	}
	binds := insp.NetworkSettings.Ports[nat.Port(fmt.Sprintf("%d/tcp", containerPort))]
	for _, b := range binds {
		if b.HostPort != "" {
			return net.JoinHostPort(b.HostIP, b.HostPort), nil
		}
	}
	return "", fmt.Errorf("container %s has no published port %d", name, containerPort)
}
