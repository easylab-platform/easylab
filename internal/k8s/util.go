package k8s

import (
	"os"
	"strings"

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
