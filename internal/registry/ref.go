// Package registry centralizes OCI/Docker image-reference handling for
// easylab: parsing, host qualification, and pull-through rewriting. Every
// image ref the gateway builds, pushes or deploys goes through here so the
// rules stay in one place.
package registry

import (
	"strconv"
	"strings"
)

// Split splits "<host>/<repo>:<tag>" into its parts. A missing host is empty,
// a missing tag defaults to "latest".
func Split(ref string) (host, repo, tag string) {
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

// Join prefixes name with host unless name already carries a registry host.
// It is the "put this repository on the local registry" primitive.
func Join(host, name string) string {
	name = strings.TrimSpace(name)
	if host == "" || name == "" {
		return name
	}
	first := name
	if i := strings.IndexByte(name, '/'); i >= 0 {
		first = name[:i]
	}
	if isHost(first) {
		return name
	}
	return host + "/" + name
}

// Local qualifies a bare image name against the local registry host (used for
// images easylab itself builds/holds). A ref that already carries a host is
// returned unchanged; a ref without a tag gets defaultTag (or "latest").
func Local(ref, host, defaultTag string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ref
	}
	first := ref
	if i := strings.IndexByte(ref, '/'); i >= 0 {
		first = ref[:i]
	}
	if isHost(first) {
		return ref
	}
	if !strings.Contains(ref, ":") {
		if defaultTag == "" {
			defaultTag = "latest"
		}
		ref += ":" + defaultTag
	}
	if host == "" {
		return ref
	}
	return host + "/" + ref
}

// PullThrough rewrites an external image reference to a pull-through ref on
// the local registry host. The node cannot reach public registries, but
// easylab's OCI registry proxies docker.io: a ref like
// "docker.io/library/nginx:alpine" becomes
// "<host>/docker.io/library/nginx:alpine" and kubelet pulls it from easylab.
// Refs already pointing at an in-cluster registry are returned unchanged.
func PullThrough(ref, host string) string {
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
	isReg := strings.Contains(first, ".") ||
		(strings.Contains(first, ":") && strings.Contains(ref, "/"))
	if !isReg {
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

// isHost reports whether a ref's first path segment is a registry host: it
// contains a '.', ends with a numeric port, or is localhost.
func isHost(seg string) bool {
	if seg == "localhost" {
		return true
	}
	if strings.Contains(seg, ".") {
		return true
	}
	if i := strings.IndexByte(seg, ':'); i >= 0 {
		port := seg[i+1:]
		if port != "" {
			if _, err := strconv.Atoi(port); err == nil {
				return true
			}
		}
	}
	return false
}
