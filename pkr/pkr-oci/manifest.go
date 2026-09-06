// Package oci implements the OCI Distribution Spec v1.1 registry protocol as
// a pull-through mirror. It mounts the spec-fixed /v2 routes and, on a local
// miss, fetches manifests/blobs from an upstream registry (default Docker
// Hub), caches them, and streams to the client.
//
// The adapter depends only on *pkrkit.Registry, so an embedder can wire any
// BlobStore / IndexStore / Upstreams implementation.
package oci

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/pkr/pkrkit"
)

// Accept is the media-types advertised for manifest GET (OCI 1.1).
const Accept = "application/vnd.oci.image.manifest.v1+json," +
	"application/vnd.docker.distribution.manifest.v2+json," +
	"application/vnd.docker.distribution.manifest.list.v2+json," +
	"application/vnd.oci.image.index.v1+json," +
	"application/vnd.oci.artifact.manifest.v1+json"

// ParseDigest validates "algo:hex" and returns the lowercase hex portion.
// sha256/sha512 only.
func ParseDigest(d string) (string, error) { return pkrkit.ParseDigest(d) }

// ReferenceIsDigest reports whether a tag-or-digest reference is a digest.
func ReferenceIsDigest(ref string) bool { return strings.Contains(ref, ":") }

// splitRegistry splits an OCI repository name into (explicit-registry,
// repository). A first component that looks like a registry host (contains a
// dot or colon, or equals "localhost") is stripped so "ghcr.io/foo/bar" and
// "foo/bar" share the same local repository.
func splitRegistry(name string) (string, string) {
	i := strings.Index(name, "/")
	if i < 0 {
		return "", name
	}
	first := name[:i]
	if strings.Contains(first, ".") || strings.Contains(first, ":") || first == "localhost" {
		return first, name[i+1 : len(name)]
	}
	return "", name
}

// extractBlobs returns the blob digests referenced by a manifest or index
// body (config + layers, or nested manifests for an index).
func extractBlobs(body []byte) []pkrkit.Descriptor {
	var probe struct {
		MediaType string `json:"mediaType"`
	}
	_ = json.Unmarshal(body, &probe)

	if probe.MediaType == "application/vnd.oci.image.index.v1+json" ||
		probe.MediaType == "application/vnd.docker.distribution.manifest.list.v2+json" {
		var idx struct {
			Manifests []struct {
				Digest    string `json:"digest"`
				MediaType string `json:"mediaType"`
				Size      int64  `json:"size"`
			} `json:"manifests"`
		}
		if json.Unmarshal(body, &idx) != nil {
			return nil
		}
		var out []pkrkit.Descriptor
		for _, m := range idx.Manifests {
			out = append(out, pkrkit.Descriptor{Digest: m.Digest, MediaType: m.MediaType, Size: m.Size})
		}
		return out
	}

	var m struct {
		Config struct {
			Digest    string `json:"digest"`
			MediaType string `json:"mediaType"`
			Size      int64  `json:"size"`
		} `json:"config"`
		Layers []struct {
			Digest    string `json:"digest"`
			MediaType string `json:"mediaType"`
			Size      int64  `json:"size"`
		} `json:"layers"`
	}
	if json.Unmarshal(body, &m) != nil {
		return nil
	}
	var out []pkrkit.Descriptor
	if m.Config.Digest != "" {
		out = append(out, pkrkit.Descriptor{Digest: m.Config.Digest, MediaType: m.Config.MediaType, Size: m.Config.Size})
	}
	for _, l := range m.Layers {
		out = append(out, pkrkit.Descriptor{Digest: l.Digest, MediaType: l.MediaType, Size: l.Size})
	}
	return out
}

// manifestSubject returns the subject digest of an OCI 1.1 referrer body.
func manifestSubject(body []byte) string {
	var probe struct {
		Subject struct {
			Digest string `json:"digest"`
		} `json:"subject"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return probe.Subject.Digest
}

// manifestArtifactType returns artifactType, falling back to config mediaType.
func manifestArtifactType(body []byte) string {
	var probe struct {
		ArtifactType string `json:"artifactType"`
		Config       struct {
			MediaType string `json:"mediaType"`
		} `json:"config"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	if probe.ArtifactType != "" {
		return probe.ArtifactType
	}
	return probe.Config.MediaType
}

var errInvalidDigest = errors.New("invalid digest")
