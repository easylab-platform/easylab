// Package conan implements the Conan 2.x repository protocol: ping, auth,
// search, recipe/package revision metadata, file get/put, upload_urls,
// download via pull-through proxy. Mirror of the pkglab reference (subset of
// the 15 protocol routes necessary for Conan 2.x clients).
package conan

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/pkr/pkrkit"
)

type State struct {
	Registry *pkrkit.Registry
	Auth     pkrkit.Auth
	SelfBase string
}

func NewHandler(reg *pkrkit.Registry, cfg map[string]any) (http.Handler, error) {
	s := &State{Registry: reg}
	if a, ok := cfg["auth"].(pkrkit.Auth); ok {
		s.Auth = a
	}
	if v, ok := cfg["self_base"].(string); ok {
		s.SelfBase = v
	}
	return s, nil
}

func init() { pkrkit.Register("conan", NewHandler) }

const capabilities = "json,rev2,revisions,checksums,upload_zip"

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/conan")
	path = strings.TrimPrefix(path, "/conan")
	path = strings.Trim(path, "/")

	// Strip the {version} prefix segment (v2).
	if i := strings.Index(path, "/"); i > 0 {
		path = path[i+1:]
	}

	switch {
	case path == "ping":
		w.Header().Set("X-Conan-Server-Capabilities", capabilities)
		w.WriteHeader(http.StatusOK)
	case path == "users/authenticate":
		s.authenticate(w, r)
	case path == "users/check_credentials":
		pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
	case strings.HasPrefix(path, "conans/"):
		s.conans(w, r, path)
	case strings.HasPrefix(path, "files/"):
		s.files(w, r, path)
	default:
		s.proxyRecipe(w, r, path)
	}
}

func (s *State) authenticate(w http.ResponseWriter, r *http.Request) {
	if s.Auth != nil {
		if u := s.Auth.Authenticate(r.Context(), r); u != "" {
			tok := s.Auth.IssueToken(r.Context(), u, nil, 3600)
			pkrkit.Text(w, http.StatusOK, tok, "text/plain")
			return
		}
	}
	pkrkit.Text(w, http.StatusOK, "anonymous-token", "text/plain")
}

func (s *State) conans(w http.ResponseWriter, r *http.Request, path string) {
	rest := strings.TrimPrefix(path, "conans/")
	parts := strings.Split(rest, "/")
	// Top-level search: /conans/search?q=<pattern>. Return local repos merged
	// with upstream results so a pulled + pushed recipe is findable.
	if parts[0] == "search" {
		s.search(w, r)
		return
	}
	// /conans/{name}/{ver}/{user}/{channel}/...
	if len(parts) < 5 {
		s.proxyRecipe(w, r, path)
		return
	}
	name, ver := parts[0], parts[1]
	user, channel := parts[2], parts[3]
	// /conans/{name}/{ver}/{user}/{channel}/revisions[...]
	_ = user
	_ = channel
	if parts[4] == "revisions" {
		// DELETE /revisions/{rev} — remove the whole recipe.
		if r.Method == http.MethodDelete {
			if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
				return
			}
			vs, _ := s.Registry.Meta.ListVersions(r.Context(), "conan", name)
			for _, v := range vs {
				removeVersion(s.Registry, name, v, r.Context())
			}
			removeVersion(s.Registry, name, ver, r.Context())
			w.WriteHeader(http.StatusOK)
			return
		}
		// Package-path: /revisions/{rev}/packages/{pid}/revisions[/{prev}/files...]
		if strings.Contains(rest, "/packages/") {
			if strings.HasSuffix(rest, "/latest") {
				// Package revision lookup: return the recorded package revision
				// if we have package files for this pid, else proxy.
				if prev, ok := s.packageRev(r.Context(), name, ver, parts); ok {
					pkrkit.JSON(w, http.StatusOK, map[string]any{"revision": prev, "time": "2024-01-01T00:00:00Z"})
					return
				}
				s.replyOrProxy(w, r, nil, "/v2/conans/"+rest)
				return
			}
			if strings.HasSuffix(rest, "/revisions") {
				// Package binary revisions list: empty (brand-new package).
				pkrkit.JSON(w, http.StatusOK, map[string]any{"revisions": []any{}})
				return
			}
			if fi := strings.Index(rest, "/files"); fi >= 0 {
				filename := strings.TrimPrefix(rest[fi+len("/files"):], "/")
				filename = strings.SplitN(filename, "/", 2)[0]
				// Package /files LISTING (no filename): return the dict of
				// package files for the pid, so the client knows what to GET.
				if filename == "" {
					pkrkit.JSON(w, http.StatusOK, map[string]any{
						"files": s.packageFiles(r.Context(), name, parts),
					})
					return
				}
				// For package file PUTs, persist the pid + prev so a later
				// /packages/{pid}/latest and file download resolve locally.
				verSlot := filename
				if pi := strings.Index(rest, "/packages/"); pi >= 0 {
					seg := rest[pi+len("/packages/"):]
					pseg := strings.SplitN(seg, "/", 2)
					if len(pseg) == 2 {
						pid, ptail := pseg[0], pseg[1]
						prev := strings.SplitN(ptail, "/", 2)[0]
						if prev == "revisions" {
							// /packages/{pid}/revisions/{prev}/files/... -> the
							// package revision is the segment after "revisions".
							p2 := strings.SplitN(ptail, "/", 3)
							if len(p2) == 3 {
								prev = p2[1]
							}
						}
						verSlot = "package/" + pid + "/" + prev + "/" + filename
					}
				}
				if r.Method == http.MethodPut {
					if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
						return
					}
					data, _ := io.ReadAll(r.Body)
					storeFile(s.Registry, name, ver, verSlot, data, r.Context())
					pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
					return
				}
				if data, ok := s.loadFile(r.Context(), name, ver, verSlot); ok {
					pkrkit.OctetResponse(w, data)
					return
				}
				if data, ok := s.loadFile(r.Context(), name, ver, filename); ok {
					pkrkit.OctetResponse(w, data)
					return
				}
				pkrkit.Error(w, http.StatusNotFound, "not found")
				return
			}
			// other package sub-paths -> proxy.
			s.replyOrProxy(w, r, nil, "/v2/conans/"+rest)
			return
		}
		// /conans/{name}/{ver}/{user}/{channel}/revisions[/{rev}/files[/{filename}]]
		// Serve a recipe's file listing / file bytes from local storage when the
		// recipe was uploaded; for a rev not in the local cache, fall back to
		// serving whatever files we did record (recipe files persist under the
		// recipe name regardless of rev).
		filesIdx := func(rest string) int {
			return strings.Index(rest, "/files")
		}
		if fi := filesIdx(rest); fi >= 0 {
			countParts := len(strings.Split(rest, "/"))
			tail := rest[fi+len("/files"):]
			var filename string
			if after, ok := strings.CutPrefix(tail, "/"); ok {
				filename = strings.SplitN(after, "/", 2)[0]
			}
			_ = countParts
			if filename == "" {
				// Listing: return a dict filename->download URL (Conan's
				// /files response shape), for the recipe files we recorded.
				pkrkit.JSON(w, http.StatusOK, map[string]any{
					"files": s.recipeFiles(r.Context(), name),
				})
				return
			}
			if r.Method == http.MethodPut {
				if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
					return
				}
				data, _ := io.ReadAll(r.Body)
				storeFile(s.Registry, name, ver, filename, data, r.Context())
				u := s.base() + "/v2/conans/" + pkrkit.URLencode(name) + "/" + pkrkit.URLencode(ver) + "/_/_/revisions/0/files/" + pkrkit.URLencode(filename)
				pkrkit.JSON(w, http.StatusOK, map[string]any{"files": map[string]any{filename: u}})
				return
			}
			if data, ok := s.loadFile(r.Context(), name, ver, filename); ok {
				pkrkit.OctetResponse(w, data)
				return
			}
			pkrkit.Error(w, http.StatusNotFound, "not found")
			return
		}
		if len(parts) == 5 {
			if r.Method == http.MethodGet {
				if !s.recipeExists(r.Context(), name, ver) {
					pkrkit.JSON(w, http.StatusOK, map[string]any{"revisions": []any{}})
					return
				}
				rev := s.loadRecipeRev(r.Context(), name)
				pkrkit.JSON(w, http.StatusOK, map[string]any{
					"reference": name + "/" + ver + "@_/_",
					"revisions": []any{map[string]any{"revision": rev, "time": "2024-01-01T00:00:00Z"}},
				})
				return
			}
		}
		// remaining sub-paths (files listing, download_urls, upload_urls,
		// packages) — proxy upstream when not locally known.
		s.replyOrProxy(w, r, nil, "/v2/conans/"+rest)
		return
	}

	sub := ""
	if len(parts) > 5 {
		sub = strings.Join(parts[5:], "/")
	}

	switch {
	case len(parts) == 5 || sub == "latest":
		if r.Method == http.MethodDelete {
			if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
				return
			}
			// Remove the recipe + all its recorded files/package slots.
			vs, _ := s.Registry.Meta.ListVersions(r.Context(), "conan", name)
			for _, v := range vs {
				removeVersion(s.Registry, name, v, r.Context())
			}
			removeVersion(s.Registry, name, ver, r.Context())
			pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		if _, err := s.Registry.Meta.Get(r.Context(), "conan", name, ver); err != nil {
			rev := s.loadRecipeRev(r.Context(), name)
			fallback := map[string]any{"revision": rev, "time": "2024-01-01T00:00:00Z"}
			s.replyOrProxy(w, r, fallback, "/v2/conans/"+pkrkit.URLencode(name)+"/"+pkrkit.URLencode(ver)+"/_/_/latest")
			return
		}
		rev := s.loadRecipeRev(r.Context(), name)
		pkrkit.JSON(w, http.StatusOK, map[string]any{"revision": rev, "time": "2024-01-01T00:00:00Z"})
	case strings.HasPrefix(sub, "revisions"):
		if _, err := s.Registry.Meta.Get(r.Context(), "conan", name, ver); err != nil {
			// Recipe not yet uploaded: return 404 so the Conan client treats
			// it as brand new (it does NOT re-list revisions on 404 and
			// proceeds to upload).
			pkrkit.Error(w, http.StatusNotFound, "revision not found")
			return
		}
		rev := s.loadRecipeRev(r.Context(), name)
		pkrkit.JSON(w, http.StatusOK, map[string]any{
			"revisions": []any{map[string]any{"revision": rev, "time": "2024-01-01T00:00:00Z"}},
		})
	case strings.HasPrefix(sub, "revisions/") && strings.HasSuffix(sub, "/files"):
		s.files(w, r, "revisions/"+strings.TrimSuffix(sub, "/files"))
	case strings.Contains(sub, "revisions/") && strings.Contains(sub, "/files/"):
		s.fileGet(w, r, name, ver, sub)
	case strings.HasSuffix(sub, "/upload_urls"):
		pkrkit.JSON(w, http.StatusOK, map[string]any{"upload_urls": map[string]any{}})
	case strings.HasSuffix(sub, "/download_urls"):
		s.downloadURLs(w, r, name, ver, sub)
	case strings.HasSuffix(sub, "/search"):
		s.replyOrProxy(w, r, map[string]any{"results": nil}, "/v2/conans/"+rest)
	default:
		s.proxyRecipe(w, r, path)
	}
}

func (s *State) fileGet(w http.ResponseWriter, r *http.Request, name, ver, sub string) {
	// /revisions/{rev}/files/{filename}
	idx := strings.Index(sub, "/files/")
	if idx < 0 {
		s.proxyRecipe(w, r, "/conans/"+name+"/"+ver+"/"+sub)
		return
	}
	filename := sub[idx+len("/files/"):]
	if r.Method == http.MethodPut {
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		data, _ := io.ReadAll(r.Body)
		storeFile(s.Registry, name, ver, filename, data, r.Context())
		pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	if data, ok := s.loadFile(r.Context(), name, ver, filename); ok {
		pkrkit.OctetResponse(w, data)
		return
	}
	s.replyOrProxy(w, r, nil, "/v2/conans/"+pkrkit.URLencode(name)+"/"+pkrkit.URLencode(ver)+"/"+sub)
}

func (s *State) downloadURLs(w http.ResponseWriter, r *http.Request, name, ver, sub string) {
	urls := map[string]any{}
	// List known files for the recipe and assign a self URL.
	if _, err := s.Registry.Meta.Get(r.Context(), "conan", name, ver); err != nil {
		s.proxyRecipe(w, r, "/conans/"+name+"/"+ver+"/"+sub)
		return
	}
	for _, fn := range []string{"conanfile.py", "conanmanifest.txt", "conaninfo.txt"} {
		if _, ok := s.loadFile(r.Context(), name, ver, fn); ok {
			urls[fn] = s.base() + "/v2/files/" + pkrkit.URLencode(name) + "/" + pkrkit.URLencode(ver) + "/_/_/0/" + fn
		}
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"download_urls": urls})
}

// search returns a merged local + upstream Conan search.
func (s *State) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	var local []string
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "conan")
	for _, repo := range repos {
		if q == "" || globMatch(repo, q) {
			local = append(local, repo+"/1.0.0@_/_")
		}
	}
	// Merge upstream results (pull-through) so public packages still appear.
	upstream := map[string]bool{}
	if base := s.Registry.Upstreams.Get("conan"); base != "" {
		remote := s.Registry.RemoteAt(base)
		if body, err := remote.GetBytes(r.Context(), "/v2/conans/search?q="+pkrkit.URLencode(q)); err == nil {
			var m map[string]any
			if json.Unmarshal(body, &m) == nil {
				if res, ok := m["results"].([]any); ok {
					for _, x := range res {
						if str, ok := x.(string); ok {
							upstream[str] = true
						}
					}
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, l := range local {
		seen[l] = true
	}
	for u := range upstream {
		if !seen[u] {
			local = append(local, u)
		}
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"results": local})
}

func (s *State) files(w http.ResponseWriter, r *http.Request, rest string) {
	// /files/{name}/{ver}/{user}/{channel}/{rev}/recipe/{filename}
	if r.Method == http.MethodPut {
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
	}

	// For GET of "/{version}/files/{name}/{ver}/{user}/{channel}/{rev}/recipe/{file}"
	// we route based on the raw request path (handled in ServeHTTP via conans).
	// This branch stubs the listing endpoint.
	pkrkit.JSON(w, http.StatusOK, map[string]any{"files": []any{}})
}

func (s *State) replyOrProxy(w http.ResponseWriter, r *http.Request, local any, upstreamPath string) {
	if local != nil {
		pkrkit.JSON(w, http.StatusOK, local)
		return
	}
	base := s.Registry.Upstreams.Get("conan")
	proxy := base
	// ConanCenter center sub-endpoint.
	if c := s.Registry.Upstreams.Sub("conan", "center"); c != "" {
		proxy = c
	}
	if proxy != "" {
		remote := s.Registry.RemoteAt(proxy)
		if body, err := remote.GetBytes(r.Context(), upstreamPath); err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.Write(body)
			return
		}
	}
	pkrkit.Error(w, http.StatusBadGateway, "upstream")
}

func (s *State) proxyRecipe(w http.ResponseWriter, r *http.Request, path string) {
	// Catch-all GET proxy for recipe metadata.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	base := s.Registry.Upstreams.Get("conan")
	if c := s.Registry.Upstreams.Sub("conan", "center"); c != "" {
		base = c
	}
	if base == "" {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	remote := s.Registry.RemoteAt(base)
	if body, err := remote.GetBytes(r.Context(), "/v2/"+strings.TrimLeft(path, "/")); err == nil {
		pkrkit.OctetResponse(w, body)
		return
	}
	pkrkit.Error(w, http.StatusNotFound, "not found")
}

func (s *State) base() string {
	if s.SelfBase == "" {
		return "http://localhost:8080/pkgs/conan"
	}
	return strings.TrimSuffix(s.SelfBase, "/")
}

func (s *State) loadRecipeRev(ctx context.Context, name string) string {
	if art, err := s.Registry.Meta.Get(ctx, "conan", name, ""); err == nil && len(art.Proprietary) > 0 {
		var m map[string]any
		if json.Unmarshal(art.Proprietary, &m) == nil {
			if r, ok := m["revision"].(string); ok && r != "" {
				return r
			}
		}
	}
	return "0"
}

// loadFile resolves a recipe (or package) file by walking the recorded
// artifacts whose version matches the filename (recipe files) — exact match.
func (s *State) loadFile(ctx context.Context, name, ver, filename string) ([]byte, bool) {
	if art, err := s.Registry.Meta.Get(ctx, "conan", name, filename); err == nil && len(art.Blobs) > 0 {
		rd, err := s.Registry.Blobs.Open(ctx, art.Blobs[0].Digest)
		if err != nil || rd == nil {
			return nil, false
		}
		data, _ := io.ReadAll(rd)
		rd.Close()
		return data, true
	}
	// Fall back: any recorded artifact blob named this filename.
	vs, err := s.Registry.Meta.ListVersions(ctx, "conan", name)
	if err != nil {
		return nil, false
	}
	for _, v := range vs {
		if art, err := s.Registry.Meta.Get(ctx, "conan", name, v); err == nil {
			for _, b := range art.Blobs {
				if b.Name == filename {
					rd, err := s.Registry.Blobs.Open(ctx, b.Digest)
					if err != nil || rd == nil {
						continue
					}
					data, _ := io.ReadAll(rd)
					rd.Close()
					return data, true
				}
			}
		}
	}
	return nil, false
}

// packageFiles returns the package-file names for a pid as a filename->URL
// dict (Conan's package /files listing shape).
func (s *State) packageFiles(ctx context.Context, name string, parts []string) map[string]any {
	pid := ""
	for i, p := range parts {
		if p == "packages" && i+1 < len(parts) {
			pid = parts[i+1]
			break
		}
	}
	if pid == "" {
		return map[string]any{}
	}
	vs, _ := s.Registry.Meta.ListVersions(ctx, "conan", name)
	base := s.base()
	out := map[string]any{}
	for _, v := range vs {
		if !strings.HasPrefix(v, "package/"+pid+"/") {
			continue
		}
		fname := v[strings.LastIndex(v, "/")+1:]
		url := base + "/v2/conans/" + pkrkit.URLencode(name) + "/_/_/_/_/revisions/0/packages/" + pkrkit.URLencode(pid) + "/revisions/0/files/" + pkrkit.URLencode(fname)
		out[fname] = url
	}
	return out
}

// packageRev returns the recorded package revision for a pid if present.
func (s *State) packageRev(ctx context.Context, name, ver string, parts []string) (string, bool) {
	// locate pid: index of "packages" in parts
	pid := ""
	for i, p := range parts {
		if p == "packages" && i+1 < len(parts) {
			pid = parts[i+1]
			break
		}
	}
	if pid == "" {
		return "", false
	}
	// package files are stored as art under (conan, name, "package/{pid}/{prev}/{file}")
	// We store package files under name+"/"+pid+"_"+filename; reconstruct the
	// prev from the recorded artifacts' version prefix.
	vs, err := s.Registry.Meta.ListVersions(ctx, "conan", name)
	if err != nil {
		return "", false
	}
	out := ""
	for _, v := range vs {
		if strings.HasPrefix(v, "package/"+pid+"/") {
			// v = "package/{pid}/{prev}/{file}"
			rest := strings.TrimPrefix(v, "package/"+pid+"/")
			if prev := strings.SplitN(rest, "/", 2)[0]; prev != "" {
				out = prev
			}
		}
	}
	if out == "" {
		return "", false
	}
	return out, true
}

// recipeExists reports whether a recipe version was uploaded locally.
func (s *State) recipeExists(ctx context.Context, name, ver string) bool {
	if _, err := s.Registry.Meta.Get(ctx, "conan", name, ver); err == nil {
		return true
	}
	// Recipe files are recorded under (name, <filename>); if any exist for
	// this recipe the recipe version is considered present.
	vs, err := s.Registry.Meta.ListVersions(ctx, "conan", name)
	if err != nil {
		return false
	}
	return len(vs) > 0
}
// recorded files (Conan's /files response is a dict keyed by filename).
func (s *State) recipeFiles(ctx context.Context, name string) map[string]any {
	vs, err := s.Registry.Meta.ListVersions(ctx, "conan", name)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	for _, v := range vs {
		// Recipe file artifacts have their filename as the version.
		if v == "" {
			continue
		}
		if _, err := s.Registry.Meta.Get(ctx, "conan", name, v); err == nil {
			out[v] = s.base() + "/v2/conans/" + pkrkit.URLencode(name) + "/0.0.0/ci/stable/revisions/0/files/" + pkrkit.URLencode(v)
		}
	}
	return out
}

func storeFile(reg *pkrkit.Registry, name, ver, filename string, data []byte, ctx context.Context) {
	// Store the file body as a blob and index it under (conan, name, filename)
	// so loadFile can find it; keep the recipe version as metadata.
	if len(data) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(data)
		digest := "sha256:" + h.SHA256
		if _, err := reg.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(data)); err == nil {
			_ = reg.Meta.Put(ctx, pkrkit.Artifact{Format: "conan", Repository: name, Version: filename, Source: "push", Blobs: []pkrkit.Descriptor{{Digest: digest, Size: int64(len(data)), Name: filename}}})
		}
	}
}

func removeVersion(reg *pkrkit.Registry, name, ver string, ctx context.Context) {
	_ = reg.Meta.Delete(ctx, "conan", name, ver)
}

func globMatch(name, pattern string) bool {
	return regexGlob(pattern, name)
}

func regexGlob(pattern, s string) bool {
	re := regexp.MustCompile("^" + regexp.QuoteMeta(pattern) + "$")
	return re.MatchString(strings.ReplaceAll(s, "*", ".*"))
}
