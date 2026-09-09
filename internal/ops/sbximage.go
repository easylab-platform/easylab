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
	"os"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/go-connections/nat"
)

// WorkerBinPath returns the easyworker binary shipped inside the easylab
// image (override with EASYLAB_WORKER_BIN). It is injected into sandbox base
// images as the ENTRYPOINT via a cached derived image.
func WorkerBinPath() string {
	if v := os.Getenv("EASYLAB_WORKER_BIN"); v != "" {
		return v
	}
	return "/usr/local/lib/easyworker/easyworker-linux-amd64"
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
	bin, err := os.ReadFile(WorkerBinPath())
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
