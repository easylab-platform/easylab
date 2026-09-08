package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/easylab-platform/artifact/core"
	artifactstore "github.com/easylab-platform/artifact/core/store"

	"github.com/easylab-platform/easyvcs/store"
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

// labTokenAuth bridges EasyVCS Lab tokens (store tokens + legacy flat-token
// set) onto artifactkit's Auth interface. A valid write-level token authenticates
// as its user name; read-level and anonymous return "" (unauthorized).
//
// When the instance is open (no registered users and no legacy token set),
// anonymous write is permitted so a fresh single-user deployment is
// frictionless, matching the Lab's labPrincipal behavior.
//
// IssueToken mints RANDOM short-lived bearer tokens bound to the grantee's
// privilege (see mintedTokens) instead of echoing a static store/flat token —
// the long-lived credential never appears in a token response.
type labTokenAuth struct {
	cs *store.CentralStore

	mintedMu sync.Mutex
	minted   map[string]*mintedLabToken
}

// mintedLabToken is an OCI bearer token issued by IssueToken.
type mintedLabToken struct {
	username string
	scopes   []string
	write    bool
	expires  time.Time
}

func newLabTokenAuth(cs *store.CentralStore) *labTokenAuth {
	return &labTokenAuth{cs: cs, minted: map[string]*mintedLabToken{}}
}

// isOpen mirrors the Lab open-instance rule: no users registered.
func (a *labTokenAuth) isOpen() bool {
	return a.cs.IsOpenInstance()
}

// resolve maps a credential (store token, minted token, or legacy flat token)
// to its username and write capability. The minted table is checked first so
// minted tokens keep their grant's privilege bounds.
func (a *labTokenAuth) resolve(token string) (username string, write bool, ok bool) {
	if a.isOpen() {
		return "open", true, true
	}
	if m, valid := a.lookupMinted(token); valid {
		return m.username, m.write, true
	}
	if t, err := a.cs.LookupToken(token); err == nil {
		w := t.Level == "write" || t.Level == "admin"
		if u, err := a.cs.GetUser(t.UserID); err == nil {
			return u.Username, w, true
		}
		return "token:" + token, w, true
	}
	return "", false, false
}

// lookupMinted resolves a minted bearer token, pruning it when expired.
func (a *labTokenAuth) lookupMinted(token string) (*mintedLabToken, bool) {
	a.mintedMu.Lock()
	defer a.mintedMu.Unlock()
	m, ok := a.minted[token]
	if !ok {
		return nil, false
	}
	if time.Now().After(m.expires) {
		delete(a.minted, token)
		return nil, false
	}
	return m, true
}

// Authenticate implements artifactkit.Auth. It returns a username for a valid
// write-level bearer token, or "" when the caller is read-only/anonymous.
func (a *labTokenAuth) Authenticate(_ context.Context, r *http.Request) string {
	if a.isOpen() {
		return "open"
	}
	raw := r.Header.Get("Authorization")
	if strings.HasPrefix(raw, "Bearer ") {
		token := strings.TrimPrefix(raw, "Bearer ")
		if u, write, ok := a.resolve(token); ok && write {
			return u
		}
	}
	return ""
}

// CheckBearer implements artifactkit.Auth for OCI scopes. The requested
// action must be permitted by the credential's level: a read-level token
// (or a pull-only mint) cannot push.
func (a *labTokenAuth) CheckBearer(_ context.Context, token, wantedScope string) (string, bool) {
	action := scopeAction(wantedScope)
	u, write, ok := a.resolve(token)
	if !ok {
		return "", false
	}
	if action == "push" || action == "delete" {
		if !write {
			return "", false
		}
		// A minted token's grant also bounds the action set.
		if m, valid := a.lookupMinted(token); valid && !actionAllowed(action, m.scopes) {
			return "", false
		}
	}
	return u, true
}

// scopeAction extracts the trailing action of an OCI scope
// ("repository:<name>:<action[,action]>"), if any.
func scopeAction(scope string) string {
	i := strings.LastIndex(scope, ":")
	if i < 0 {
		return ""
	}
	act := scope[i+1:]
	if j := strings.Index(act, ","); j >= 0 {
		act = act[:j]
	}
	return act
}

// actionAllowed reports whether the action appears in the minted grant's
// scopes ("pull" is always allowed for a valid mint).
func actionAllowed(action string, scopes []string) bool {
	if action == "" || action == "pull" {
		return true
	}
	for _, s := range scopes {
		if i := strings.LastIndex(s, ":"); i >= 0 {
			for _, a := range strings.Split(s[i+1:], ",") {
				if a == action || a == "*" {
					return true
				}
			}
		}
	}
	return false
}

// CheckToken implements artifactkit.Auth for raw tokens (cargo/npm/publish).
func (a *labTokenAuth) CheckToken(_ context.Context, token string) (string, bool) {
	return a.CheckBearer(context.Background(), token, "")
}

// CheckBasic implements artifactkit.Auth for basic auth (user:token).
func (a *labTokenAuth) CheckBasic(_ context.Context, _, pass string) bool {
	_, ok := a.CheckBearer(context.Background(), pass, "")
	return ok
}

// IssueToken implements artifactkit.Auth. It mints a RANDOM bearer token
// bound to the grantee's privilege (write tokens get a push-capable mint,
// everyone else a pull-only mint). The static store/flat credential is never
// returned to a client.
func (a *labTokenAuth) IssueToken(ctx context.Context, username string, scopes []string, ttl time.Duration) string {
	if ttl <= 0 {
		ttl = time.Hour
	}
	if username == "" {
		return ""
	}
	// Determine the grantee's write capability from the store realm.
	write := false
	if u, err := a.cs.GetUserByUsername(username); err == nil {
		toks, err := a.cs.ListTokens(u.ID)
		if err == nil {
			for _, t := range toks {
				if t.Level == "write" || t.Level == "admin" {
					write = true
					break
				}
			}
		}
	} else if username == "open" {
		// Open instance: anonymous write allowed.
		write = true
	}
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is unrecoverable; fail closed (no token).
		return ""
	}
	tok := "lab_" + base64.RawURLEncoding.EncodeToString(b[:])
	a.mintedMu.Lock()
	// Opportunistically prune expired mints so the table stays bounded.
	now := time.Now()
	for k, m := range a.minted {
		if now.After(m.expires) {
			delete(a.minted, k)
		}
	}
	a.minted[tok] = &mintedLabToken{username: username, scopes: scopes, write: write, expires: now.Add(ttl)}
	a.mintedMu.Unlock()
	return tok
}
