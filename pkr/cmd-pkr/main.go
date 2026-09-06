// Command pkr runs a multi-protocol package registry. Each protocol is a
// separate module that registers itself with pkrkit at compile time (see the
// blank imports in adapters). The server can mount a single protocol or many,
// so a pull-through mirror may be started per-protocol or all-at-once.
//
// Example:
//
//	pkr server --protocols=oci,pypi --listen :8080 --data ./data
//	pkr server --protocols=oci --oci.upstream https://registry-1.docker.io
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/pkr/pkrkit"
	"github.com/pkr/pkrkit/store"
	_ "github.com/pkr/cmd-pkr/adapters" // registers all enabled protocols
)

func main() {
	var (
		listen     = flag.String("listen", ":8080", "HTTP listen address")
		dataDir    = flag.String("data", "./data", "substrate root (sqlite + blobs + upstreams)")
		protocols  = flag.String("protocols", "", "comma-separated protocols to mount (default: all registered)")
		selfBase   = flag.String("self-base", "", "external base URL for auth realms / self URIs")
		tokens     = flag.String("tokens", "", "static `token=level` pairs (read|write), comma separated")
		airGap     = flag.Bool("air-gap", false, "disable all upstream pull-through")
		blobBackend = flag.String("blob-backend", "file", "blob backend: file | sqlite")
	)
	flag.Parse()

	var auth pkrkit.Auth
	if *tokens != "" {
		auth = pkrkit.NewTokenAuth(*tokens)
	}

	upstreams := defaultUpstreams(*airGap)

	// Open the metadata store (SQLite) always; blob backend is chosen below.
	idxPath := filepath.Join(*dataDir, "pkglab.db")
	meta, err := store.OpenSQLite(idxPath)
	if err != nil {
		log.Fatalf("open metadata: %v", err)
	}
	defer meta.Close()

	var blobs pkrkit.BlobStore
	switch *blobBackend {
	case "sqlite":
		blobs = store.NewSQLiteBlobStore(meta.DB())
	default:
		b, err := store.NewFileBlobStore(filepath.Join(*dataDir, "blobs"))
		if err != nil {
			log.Fatalf("open blob store: %v", err)
		}
		blobs = b
	}

	reg := &pkrkit.Registry{Blobs: blobs, Meta: meta, Upstreams: upstreams}

	// Determine which protocols to mount.
	names := *protocols
	if names == "" {
		all := pkrkit.Registered()
		names = strings.Join(all, ",")
	}
	mux := http.NewServeMux()
	mounted := 0
	for _, name := range strings.Split(names, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		handler, err := pkrkit.Build(name, reg, configFor(name, *selfBase, auth))
		if err != nil {
			log.Fatalf("build protocol %q: %v", name, err)
		}
		// OCI is spec-fixed at /v2; everything else under /pkgs/<name>.
		if name == "oci" {
			// Wire the /token auth endpoint the OCI challenges reference.
			// jjlab serves it under /v2/token (the /v2 mount), so expose both.
			if auth != nil {
				tokenH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					serveToken(w, r, auth)
				})
				mux.Handle("/v2/token", tokenH)
				mux.Handle("/token", tokenH)
			}
			mux.Handle("/v2", handler)
			mux.Handle("/v2/", handler)
		} else {
			mux.Handle("/pkgs/"+name+"/", handler)
			mux.Handle("/pkgs/"+name, handler)
		}
		mounted++
	}
	if mounted == 0 {
		log.Fatal("no protocols registered/enabled")
	}

	addr := *listen
	log.Printf("pkr listening on %s (%d protocols: %s), data=%s, airgap=%v",
		addr, mounted, names, *dataDir, *airGap)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func defaultUpstreams(airGap bool) *pkrkit.Upstreams {
	return &pkrkit.Upstreams{
		Defaults: map[string]string{
			"oci":     "https://registry-1.docker.io",
			"cargo":   "https://crates.io",
			"composer": "https://repo.packagist.org",
			"conan":   "https://center.conan.io",
			"go":      "https://proxy.golang.org",
			"helm":    "https://charts.helm.sh/stable",
			"hex":     "https://repo.hex.pm",
			"maven":   "https://repo.maven.apache.org/maven2",
			"npm":     "https://registry.npmjs.org",
			"nuget":   "https://api.nuget.org",
			"pub":     "https://pub.dev",
			"pypi":    "https://pypi.org",
			"rubygems": "https://rubygems.org",
			"swift":   "https://api.spm.swift.org",
			// Sub-endpoints that live on a different host than the format's
			// primary upstream (crates.io: index + static downloads are
			// served from index.crates.io / static.crates.io).
			"cargo.index":  "https://index.crates.io",
			"cargo.static": "https://static.crates.io/crates",
			"conan.center": "https://center2.conan.io",
			"nuget.search": "https://azuresearch-usnc.nuget.org",
			"nuget.registration": "https://api.nuget.org",
			"hex.repo":  "https://repo.hex.pm",
			"rubygems.index": "https://index.rubygems.org",
			"rubygems.gems":  "https://rubygems.org/gems",
		},
		Overrides: map[string]string{},
		Proxy:     map[string]string{},
		AirGap:    airGap,
	}
}

func configFor(name, selfBase string, auth pkrkit.Auth) map[string]any {
	cfg := map[string]any{}
	// Each protocol mounts under /pkgs/<name> (OCI is special-cased to /v2),
	// so its emitted self-URLs must carry that prefix. selfBase is the global
	// origin (scheme://host[:port]). For OCI, SelfBase is used ONLY to derive
	// the /token realm, which lives at the origin root — so pass the bare base.
	if selfBase != "" {
		if name == "oci" {
			cfg["self_base"] = strings.TrimSuffix(selfBase, "/")
		} else {
			cfg["self_base"] = strings.TrimSuffix(selfBase, "/") + "/pkgs/" + name
		}
	}
	if auth != nil {
		cfg["auth"] = auth
	}
	return cfg
}

// serveToken issues an OCI bearer token from the configured auth.
func serveToken(w http.ResponseWriter, r *http.Request, auth pkrkit.Auth) {
	scopes := collectScopes(r.URL.Query()["scope"])
	username := auth.Authenticate(r.Context(), r)
	if !canPush(scopes) || username != "" {
		tok := auth.IssueToken(r.Context(), username, scopes, 3600)
		writeJSON(w, map[string]any{
			"token": tok, "access_token": tok, "expires_in": 3600,
		})
		return
	}
	if username == "" && canPush(scopes) {
		w.Header().Set("WWW-Authenticate", `Basic realm="/token"`)
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]any{"errors": []any{
			map[string]any{"code": "UNAUTHORIZED", "message": "authentication required"},
		}})
		return
	}
}

func collectScopes(vals []string) []string {
	var out []string
	for _, v := range vals {
		for _, s := range strings.Fields(v) {
			if s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

func canPush(scopes []string) bool {
	for _, s := range scopes {
		if i := strings.LastIndex(s, ":"); i >= 0 {
			act := s[i+1:]
			// An OCI scope may carry comma-separated actions ("pull,push").
			for _, a := range strings.Split(act, ",") {
				if a == "push" || a == "delete" || a == "*" {
					return true
				}
			}
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, "%s", mustJSON(v))
}

func mustJSON(v any) string {
	b, err := jsonMarshal(v)
	if err != nil {
		return `{}`
	}
	return string(b)
}
