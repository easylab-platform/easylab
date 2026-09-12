package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/easylab-platform/easylab/internal/k8s"
)

// ensureSandboxImage makes sure a derived sandbox image (base + injected
// worker) exists in easylab's OCI registry, building it with an ephemeral
// buildkit pod when missing. Returns (imageRef, built). The derived tag is
// deterministic over (base, worker binary) so it is built once per pair.
func (s *server) ensureSandboxImage(ctx context.Context, baseImage, workspace string) (string, bool, error) {
	if s.k8s == nil {
		return "", false, fmt.Errorf("k8s backend unavailable")
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
	if s.imageExists(ctx, tag) {
		return tag, false, nil
	}
	if err := s.k8s.BuildImage(ctx, k8s.BuildOptions{
		ContextDir: ctxDir, Dockerfile: "Containerfile", Image: tag,
	}, nil); err != nil {
		return "", false, fmt.Errorf("derive build: %w", err)
	}
	return tag, true, nil
}

// sandboxRegistryRef returns the fully-qualified derived sandbox image ref.
// The host is the TLS ingress name the node already trusts, so kubelet pulls
// the derived image anonymously over HTTPS without any cluster registry
// configuration or pull secret.
func (s *server) sandboxRegistryRef(short string) string {
	host := envOrStr("EASYLAB_REGISTRY_HOST", "easylab.temp.10.199.64.20.nip.io")
	return host + "/easylab/sandbox:" + short
}

// imageExists reports whether the registry already has the image tag
// (best effort: a missing repo/tag is a 404; any other failure rebuilds). The
// probe uses the in-cluster service URL (always reachable from the pod), while
// the returned image ref uses the node-facing TLS host.
func (s *server) imageExists(ctx context.Context, ref string) bool {
	_, repo, tag := splitRef(ref)
	base := envOrStr("EASYLAB_ARTIFACT_URL", "http://easylab.temp.svc.cluster.local:80")
	u := strings.TrimSuffix(base, "/") + "/v2/" + repo + "/manifests/" + tag
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+envOrStr("EASYVCS_TOKEN", "devtoken"))
	req.Header.Set("Accept", "application/vnd.docker.distribution.manifest.v2+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// splitRef splits <host>/<repo>:<tag> into its parts (tag defaults to latest).
func splitRef(ref string) (host, repo, tag string) {
	host, rest := "", ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		host, rest = ref[:i], ref[i+1:]
	}
	tag = "latest"
	if i := strings.LastIndexByte(rest, ':'); i >= 0 {
		rest, tag = rest[:i], rest[i+1:]
	}
	return host, rest, tag
}

func workerBinPath() string {
	if v := os.Getenv("EASYLAB_WORKER_BIN"); v != "" {
		return v
	}
	return "/usr/local/lib/easyworker/easyworker-linux-amd64"
}
