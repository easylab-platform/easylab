package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/easylab-platform/artifact/core"
	pkrstore "github.com/easylab-platform/artifact/core/store"

	"github.com/easylab-platform/easyvcs/store"
)

// openRegistry builds the pkrkit package registry substrate rooted under the
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
func openRegistry(home string) (*pkrkit.Registry, error) {
	root := filepath.Join(home, "registry")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	// Metadata: switchable (shares EASYVCS_DB_* with the easyvcs engine).
	idx, err := pkrstore.OpenStore(pkrstore.DriverConfig{
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
	blobs, err := pkrstore.OpenBlobStore(blobBackend, filepath.Join(root, "blobs"))
	if err != nil {
		return nil, err
	}
	airGap := os.Getenv("EASYVCS_AIRGAP") == "1"
	return &pkrkit.Registry{
		Blobs: blobs,
		Meta:  idx,
		Upstreams: &pkrkit.Upstreams{
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

// defaultUpstreams mirrors the pkr reference defaults, mapping each package
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

// labTokenAuth bridges EasyVCS Lab tokens (store tokens + legacy flat-token
// set) onto pkrkit's Auth interface. A valid write-level token authenticates
// as its user name; read-level and anonymous return "" (unauthorized).
//
// When the instance is open (no registered users and no legacy token set),
// anonymous write is permitted so a fresh single-user deployment is
// frictionless, matching the Lab's labPrincipal behavior.
type labTokenAuth struct {
	cs     *store.CentralStore
	tokens map[string]bool
}

// isOpen mirrors the Lab open-instance rule: no users and no legacy tokens.
func (a *labTokenAuth) isOpen() bool {
	if a.tokens != nil && len(a.tokens) > 0 {
		return false
	}
	users, err := a.cs.ListUsers()
	return err == nil && len(users) == 0
}

// Authenticate implements pkrkit.Auth. It returns a username for a valid
// write-level bearer token, or "" when the caller is read-only/anonymous.
func (a *labTokenAuth) Authenticate(_ context.Context, r *http.Request) string {
	if a.isOpen() {
		return "open"
	}
	raw := r.Header.Get("Authorization")
	if strings.HasPrefix(raw, "Bearer ") {
		token := strings.TrimPrefix(raw, "Bearer ")
		if t, err := a.cs.LookupToken(token); err == nil {
			if t.Level == "write" || t.Level == "admin" {
				if u, err := a.cs.GetUser(t.UserID); err == nil {
					return u.Username
				}
				return "token:" + token
			}
			return ""
		}
		if a.tokens != nil && a.tokens[token] {
			return "token:" + token
		}
	}
	return ""
}

// CheckBearer implements pkrkit.Auth for OCI scopes.
func (a *labTokenAuth) CheckBearer(_ context.Context, token, _ string) (string, bool) {
	if a.isOpen() {
		return "open", true
	}
	if t, err := a.cs.LookupToken(token); err == nil && (t.Level == "write" || t.Level == "admin") {
		if u, err := a.cs.GetUser(t.UserID); err == nil {
			return u.Username, true
		}
		return "token:" + token, true
	}
	if a.tokens != nil && a.tokens[token] {
		return "token:" + token, true
	}
	return "", false
}

// CheckToken implements pkrkit.Auth for raw tokens (cargo/npm/publish).
func (a *labTokenAuth) CheckToken(_ context.Context, token string) (string, bool) {
	return a.CheckBearer(context.Background(), token, "")
}

// CheckBasic implements pkrkit.Auth for basic auth (user:token).
func (a *labTokenAuth) CheckBasic(_ context.Context, _, pass string) bool {
	_, ok := a.CheckBearer(context.Background(), pass, "")
	return ok
}

// IssueToken implements pkrkit.Auth. The Lab realm uses static tokens as
// credentials, so we return a valid write-level token for the authenticated
// principal (the client will present it as a Bearer token on subsequent
// requests, which CheckBearer/CheckToken validate against the store).
func (a *labTokenAuth) IssueToken(ctx context.Context, username string, _ []string, _ time.Duration) string {
	if username == "" {
		return ""
	}
	// Legacy flat-token realm: return any registered write token verbatim.
	if a.tokens != nil {
		for t := range a.tokens {
			return t
		}
	}
	// Lab token realm: return the first write token owned by this user.
	u, err := a.cs.GetUserByUsername(username)
	if err != nil {
		return ""
	}
	toks, err := a.cs.ListTokens(u.ID)
	if err != nil {
		return ""
	}
	for _, t := range toks {
		if t.Level == "write" || t.Level == "admin" {
			return t.Token
		}
	}
	// Fall back to any token (read) so read-scoped flows can proceed.
	if len(toks) > 0 {
		return toks[0].Token
	}
	return ""
}
