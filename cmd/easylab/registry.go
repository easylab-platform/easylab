package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/easylab-platform/artifact/core"
	artifactstore "github.com/easylab-platform/artifact/core/store"
)

// openRegistry builds the artifactkit package registry substrate rooted under the
// EasyVCS home dir. Metadata uses a switchable backend shared with the central
// store (EASYVCS_DB_DRIVER / EASYVCS_DB_DSN; sqlite default, postgres/mysql), so
// no external process is required for the default. Artifact bytes live on the
// filesystem (EASYVCS_BLOB_BACKEND=filesystem default; s3 is a placeholder that
// falls back to filesystem). The registry backs Lab "releases" (generic format)
// and, when mounted, language-package protocols.
//
// Pull-through upstreams are enabled by default so clients can pull packages
// from their public upstreams (npm, pypi, crates.io, ...) and cache them
// locally. Set EASYVCS_AIRGAP=1 to disable all upstreams (local-only).
func openRegistry(home string) (*artifactkit.Registry, error) {
	root := filepath.Join(home, "registry")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	// Metadata: switchable (shares EASYVCS_DB_* with the easyvcs engine).
	idx, err := artifactstore.OpenStore(artifactstore.DriverConfig{
		Kind: envOrStr("EASYVCS_DB_DRIVER", "sqlite"),
		DSN:  dbDSNOr(filepath.Join(root, "registry.db")),
	})
	if err != nil {
		return nil, err
	}
	// Blob content: filesystem default; "s3" is a placeholder that falls back to
	// the filesystem CAS so a misconfigured deployment never fails to start.
	blobBackend := strings.ToLower(os.Getenv("EASYVCS_BLOB_BACKEND"))
	if blobBackend == "" {
		blobBackend = "filesystem"
	}
	blobs, err := artifactstore.OpenBlobStore(blobBackend, filepath.Join(root, "blobs"))
	if err != nil {
		return nil, err
	}
	airGap := os.Getenv("EASYVCS_AIRGAP") == "1"
	return &artifactkit.Registry{
		Blobs: blobs,
		Meta:  idx,
		Upstreams: &artifactkit.Upstreams{
			Defaults:  defaultUpstreams(),
			Overrides: map[string]string{},
			Proxy:     map[string]string{},
			AirGap:    airGap,
		},
	}, nil
}

// dbDSNOr returns the metadata DSN: EASYVCS_DB_DSN if set, else the given
// sqlite path (used for the default sqlite backend).
func dbDSNOr(def string) string {
	if d := os.Getenv("EASYVCS_DB_DSN"); d != "" {
		return d
	}
	return def
}

// defaultUpstreams mirrors the artifact reference defaults, mapping each package
// format (and its sub-endpoints) to its public upstream base URL.
func defaultUpstreams() map[string]string {
	return map[string]string{
		"oci":                "https://registry-1.docker.io",
		"cargo":              "https://crates.io",
		"composer":           "https://repo.packagist.org",
		"conan":              "https://center.conan.io",
		"go":                 "https://proxy.golang.org",
		"helm":               "https://charts.helm.sh/stable",
		"hex":                "https://repo.hex.pm",
		"maven":              "https://repo.maven.apache.org/maven2",
		"npm":                "https://registry.npmjs.org",
		"nuget":              "https://api.nuget.org",
		"pub":                "https://pub.dev",
		"pypi":               "https://pypi.org",
		"rubygems":           "https://rubygems.org",
		"swift":              "https://api.spm.swift.org",
		"cargo.index":        "https://index.crates.io",
		"cargo.static":       "https://static.crates.io/crates",
		"conan.center":       "https://center2.conan.io",
		"nuget.search":       "https://azuresearch-usnc.nuget.org",
		"nuget.registration": "https://api.nuget.org",
		"hex.repo":           "https://repo.hex.pm",
		"rubygems.index":     "https://index.rubygems.org",
		"rubygems.gems":      "https://rubygems.org/gems",
	}
}
