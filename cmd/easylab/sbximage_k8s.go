package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/easylab-platform/easylab/internal/ci"
	"github.com/easylab-platform/easylab/internal/k8s"
	"github.com/easylab-platform/easylab/internal/registry"
)

// ensureSandboxImage resolves the image for a sandbox runtime. For a derived
// profile (linux) it builds base+worker and pushes to easylab's registry; for
// a VM profile (windows/macos) the profile's prebuilt image is returned as-is.
// Returns (imageRef, built).
func (s *server) ensureSandboxImage(ctx context.Context, profile k8s.RuntimeProfile, baseImage, workspace string) (string, bool, error) {
	if s.k8s == nil {
		return "", false, fmt.Errorf("k8s backend unavailable")
	}
	if !profile.Derived {
		if profile.Image == "" {
			return "", false, fmt.Errorf("runtime %q has no image configured", profile.Name)
		}
		return profile.Image, false, nil
	}
	bin, err := os.ReadFile(workerBinPath())
	if err != nil {
		return "", false, fmt.Errorf("worker binary: %w", err)
	}
	sum := sha256.Sum256(append([]byte(baseImage+"|"), bin...))
	short := hex.EncodeToString(sum[:])[:16]
	tag := s.sandboxRegistryRef(short)

	// Build context: worker binary + Containerfile. Lives under /data so the
	// build pod's hostPath mount can read it.
	ctxDir := filepath.Join(k8s.DataDir(), "buildctx", "sbx-"+short)
	if err := os.MkdirAll(ctxDir, 0o755); err != nil {
		return "", false, err
	}
	if err := os.WriteFile(filepath.Join(ctxDir, "easyworker"), bin, 0o755); err != nil {
		return "", false, err
	}
	if workspace == "" {
		workspace = "/workspace"
	}
	containerfile := fmt.Sprintf(`FROM %s
COPY easyworker /usr/local/bin/easyworker
ENV WORKER_PORT=48080
WORKDIR %s
ENTRYPOINT ["/usr/local/bin/easyworker"]
`, baseImage, workspace)
	if err := os.WriteFile(filepath.Join(ctxDir, "Containerfile"), []byte(containerfile), 0o644); err != nil {
		return "", false, err
	}

	// Skip the build when the tag already resolves in the registry.
	if s.k8s.ImageExists(ctx, tag) {
		return tag, false, nil
	}
	if err := s.deriveImage(ctx, ctxDir, tag); err != nil {
		return "", false, fmt.Errorf("derive build: %w", err)
	}
	return tag, true, nil
}

// deriveImage builds a derived sandbox image through the shared CI produce
// path (oci-build), so sandbox image derivation, container-build and
// package-publish all run through the one orchestrator (K8sBackend -> the
// unified worker-job primitive).
func (s *server) deriveImage(ctx context.Context, ctxDir, tag string) error {
	if s.buildBackend == nil {
		return fmt.Errorf("build backend unavailable")
	}
	_, err := s.buildBackend.Run(ctx, &ci.Job{
		Produce: ci.Produce{
			Action:     ci.ActionOCIBuild,
			Context:    ctxDir,
			Dockerfile: "Containerfile",
			Tag:        tag,
		},
	}, nil, nil)
	return err
}

// sandboxRegistryRef returns the fully-qualified derived sandbox image ref.
// The host comes from the k8s client's registry host (in-cluster Service DNS
// by default, or EASYLAB_REGISTRY_HOST), so it is portable across clusters.
func (s *server) sandboxRegistryRef(short string) string {
	host := s.registryHost()
	return registry.Join(host, "easylab/sandbox:"+short)
}

// registryHost resolves the registry host: an explicit EASYLAB_REGISTRY_HOST
// wins, then the k8s client's configured host, then the namespace-derived
// in-cluster Service DNS.
func (s *server) registryHost() string {
	if v := os.Getenv("EASYLAB_REGISTRY_HOST"); v != "" {
		return v
	}
	if s.k8s != nil && s.k8s.RegistryHost() != "" {
		return s.k8s.RegistryHost()
	}
	ns := envOrStr("EASYLAB_NAMESPACE", "temp")
	return fmt.Sprintf("easylab.%s.svc.cluster.local:80", ns)
}

func workerBinPath() string {
	if v := os.Getenv("EASYLAB_WORKER_BIN"); v != "" {
		return v
	}
	return "/usr/local/lib/easyworker/easyworker-linux-amd64"
}

// workerHostPath returns the hostPath of the worker binary (published in the
// image under /data), so job/build pods can mount it from the node without a
// per-job derived image. Best effort: empty when the host data dir is unknown
// or the binary is absent.
func workerHostPath() string {
	root := os.Getenv("EASYLAB_HOST_DATA_DIR")
	if root == "" {
		return ""
	}
	p := filepath.Join(root, "worker", "easyworker")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// workerHostDir returns the hostPath directory holding the worker binary.
func workerHostDir() string {
	p := workerHostPath()
	if p == "" {
		return ""
	}
	return filepath.Dir(p)
}

// publishWorker copies the worker binary to /data/worker/easyworker so the
// hostPath is available to build/job pods. Idempotent.
func publishWorker(bin []byte) error {
	dir := filepath.Join(k8s.DataDir(), "worker")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := filepath.Join(dir, "easyworker")
	if err := os.WriteFile(p, bin, 0o755); err != nil {
		return err
	}
	// Match the rootless buildkit job's uid 1000/10000 so the binary is
	// executable there.
	_ = os.Chmod(p, 0o755)
	_ = os.Chmod(dir, 0o755)
	return nil
}
