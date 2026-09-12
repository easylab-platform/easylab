package k8s

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// DataDir returns easylab's in-container data root (the /data hostPath mount).
func DataDir() string { return dataDir() }

// dataDir returns easylab's in-container data root (the /data hostPath mount).
func dataDir() string {
	if v := os.Getenv("EASYVCS_HOME"); v != "" {
		return v
	}
	return "/data"
}

// hostDataRoot returns the node path backing /data (the easylab hostPath).
func (c *Client) hostDataRoot() (string, error) {
	if v := os.Getenv("EASYLAB_HOST_DATA_DIR"); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("EASYLAB_HOST_DATA_DIR unset (host path of the easylab /data mount)")
}

// hostPath maps an in-container data path (/data/...) to its node (host) path.
func (c *Client) hostPath(containerPath string) string {
	root, _ := c.hostDataRoot()
	dd := dataDir()
	rel := containerPath
	if len(rel) >= len(dd) && rel[:len(dd)] == dd {
		rel = rel[len(dd):]
	}
	return filepath.Join(root, rel)
}

// randomToken mints a per-job worker bearer token.
func (c *Client) randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// proxyVars returns the HTTP(S) proxy env as a map for job env injection.
func (c *Client) proxyVars() map[string]string {
	if c.proxy == "" {
		return nil
	}
	return map[string]string{
		"HTTP_PROXY":  c.proxy,
		"HTTPS_PROXY": c.proxy,
		"NO_PROXY":    "localhost,127.0.0.1,.svc.cluster.local,.svc," + c.registryHost,
	}
}

// buildResources returns requests+limits (the namespace quota requires both).
func buildResources(cpu, mem string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		},
	}
}

// DefaultResources returns the requests+limits required by the namespace's
// ResourceQuota (which mandates cpu/memory requests AND limits on every
// container).
func DefaultResources() corev1.ResourceRequirements { return defaultResources() }

// defaultResources returns the requests+limits required by the namespace's
// ResourceQuota (which mandates cpu/memory requests AND limits on every
// container).
func defaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
}

func hpPtr(t corev1.HostPathType) *corev1.HostPathType { return &t }
func boolPtr(b bool) *bool                             { return &b }

// cacheRefFor derives the registry cache tag for an image ref
// (<host>/<repo>:<tag> -> <host>/easylab-cache/<repo>:<tag>).
func (c *Client) cacheRefFor(image string) string {
	name := image
	host := ""
	if i := strings.IndexByte(name, '/'); i >= 0 {
		host = name[:i]
	}
	tag := "latest"
	if i := strings.LastIndexByte(name, ':'); i >= 0 && i > strings.LastIndexByte(name, '/') {
		tag = name[i+1:]
		name = name[:i]
	}
	repo := name
	if host != "" {
		repo = name[len(host)+1:]
	}
	return fmt.Sprintf("%s/easylab-cache/%s:%s", c.registryHost, repo, tag)
}

var _ = time.Second

func joinLines(xs []string) string { return strings.Join(xs, "\n") }

func splitLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

func stringsContains(s, sub string) bool { return strings.Contains(s, sub) }

// RewriteImageRef rewrites an external image reference to a pull-through ref
// on the local registry host. The node cannot reach public registries, but
// easylab's OCI registry proxies docker.io: a ref like
// "docker.io/library/nginx:alpine" becomes
// "<host>/docker.io/library/nginx:alpine" and kubelet pulls it from easylab.
// Refs already pointing at an in-cluster registry are returned unchanged.
func RewriteImageRef(ref, host string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" || host == "" {
		return ref
	}
	first := ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		first = ref[:i]
	}
	// Local / in-cluster registries must not be prefixed.
	if first == host || first == "localhost" || strings.HasPrefix(first, "localhost:") {
		return ref
	}
	if strings.Contains(first, ".svc") {
		return ref
	}
	// A Docker Hub repo has no registry host: either a bare "name[:tag]" (no
	// slash at all) or a "path/name[:tag]" whose first segment has no '.'/':'.
	isRegistry := strings.Contains(first, ".") ||
		(strings.Contains(first, ":") && strings.Contains(ref, "/"))
	if !isRegistry {
		// Docker Hub shorthand. A bare "name[:tag]" is normalized to
		// "library/name" (Docker Hub's rule), which the pull-through proxy
		// resolves against docker.io.
		if strings.Contains(ref, "/") {
			return host + "/docker.io/" + ref
		}
		return host + "/docker.io/library/" + ref
	}
	// A registry host (docker.io, ghcr.io, quay.io, host:port, ...): prefix it
	// so the pull goes through easylab's pull-through proxy.
	return host + "/" + ref
}
