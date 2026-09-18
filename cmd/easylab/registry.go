package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/easylab-platform/artifact/core"
	artifactstore "github.com/easylab-platform/artifact/core/store"
	"github.com/easylab-platform/artifact/targets"
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
	// The target registry is the single source of truth for public upstreams
	// and mirrors (shared with artifact and easysidecar). The flat Defaults map
	// is a projection so the admin API and per-repo overrides keep working.
	targetReg := targets.NewRegistry()
	// User-declared targets persist in the metadata DB; a load failure is not
	// fatal (the built-in table is a working registry).
	if err := targetReg.Load(context.Background(), idx); err != nil {
		log.Printf("targets: load user targets: %v", err)
	}
	return &artifactkit.Registry{
		Blobs: blobs,
		// The scoped store applies the request's repository namespace to every
		// metadata read/write. Handlers without a request scope (the Lab API's
		// own listings, background jobs) pass through unchanged.
		Meta:        artifactkit.NewScopedStore(idx),
		TargetStore: idx,
		Upstreams: &artifactkit.Upstreams{
			Targets:   targetReg,
			Defaults:  targetReg.LegacyDefaults(),
			Overrides: map[string]string{},
			Proxy:     map[string]string{},
			Repos:     map[string]artifactkit.RepoUpstream{},
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

