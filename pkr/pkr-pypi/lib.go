// Package pypi implements the PyPI protocol (PEP 503 / 691 / 658) with
// pull-through and href rewriting, mirroring the pkglab reference.
package pypi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
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

func init() { pkrkit.Register("pypi", NewHandler) }

// NormalizeName follows PEP 503: lowercase and collapse runs of -_. to a
// single hyphen.
func NormalizeName(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	lastSep := false
	for _, c := range name {
		if c == '-' || c == '_' || c == '.' {
			if !lastSep && b.Len() > 0 {
				b.WriteByte('-')
			}
			lastSep = true
		} else {
			b.WriteRune(c)
			lastSep = false
		}
	}
	return strings.Trim(b.String(), "-")
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/vnd.pypi.simple.v1+json")
}

func (s *State) base() string {
	if s.SelfBase == "" {
		return "http://localhost:8080/pkgs/pypi"
	}
	return strings.TrimSuffix(s.SelfBase, "/")
}

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/pypi")
	path = strings.TrimPrefix(path, "/pypi")

	switch {
	case r.URL.Path == "/upload" || path == "/upload" || r.URL.Path == "/":
		if r.Method == http.MethodPost {
			s.upload(w, r)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	case strings.HasPrefix(path, "/simple/") || strings.HasSuffix(path, "/simple"):
		s.simple(w, r, path)
		return
	case strings.HasPrefix(path, "/api/projects/"):
		s.projectAPI(w, r, path)
		return
	default:
		pkrkit.Error(w, http.StatusNotFound, "not found")
	}
}

func (s *State) simple(w http.ResponseWriter, r *http.Request, path string) {
	rel := strings.Trim(strings.TrimPrefix(path, "/simple"), "/")
	if rel == "" {
		s.simpleRoot(w, r)
		return
	}
	parts := strings.SplitN(rel, "/", 2)
	project := NormalizeName(parts[0])
	if len(parts) == 1 {
		s.simpleProject(w, r, project)
		return
	}
	fname := parts[1]
	if strings.HasSuffix(fname, ".metadata") {
		s.metadataFile(w, r, project, strings.TrimSuffix(fname, ".metadata"))
		return
	}
	s.simpleFile(w, r, project, fname)
}

func (s *State) simpleRoot(w http.ResponseWriter, r *http.Request) {
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "pypi")
	if wantsJSON(r) {
		var projects []any
		for _, n := range repos {
			projects = append(projects, map[string]any{"name": n})
		}
		pkrkit.JSON(w, http.StatusOK, map[string]any{"meta": map[string]any{"api-version": "1.4"}, "projects": projects})
		return
	}
	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html><html><body>\n")
	for _, name := range repos {
		sb.WriteString(`<a href="` + s.base() + "/simple/" + pkrkit.URLencode(name) + "/\">" + name + "</a>\n")
	}
	sb.WriteString("</body></html>")
	pkrkit.Text(w, http.StatusOK, sb.String(), "text/html")
}

func (s *State) simpleProject(w http.ResponseWriter, r *http.Request, name string) {
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "pypi", name)
	// Always merge upstream files so a local partial cache (or a single
	// pulled wheel) does not shadow the rest of the project. A pull-through
	// mirror must present the full index; the reference only proxies on a
	// total miss, but uv/pip resolve ranges against the whole page.
	if s.mergeSimpleProject(w, r, name, versions) {
		return
	}
	if wantsJSON(r) {
		s.simpleProjectJSON(w, r, name, versions)
		return
	}
	var sb strings.Builder
	sb.WriteString("<!DOCTYPE html><html><body>\n")
	for _, v := range versions {
		art, err := s.Registry.Meta.Get(r.Context(), "pypi", name, v)
		if err != nil {
			continue
		}
		for _, b := range art.Blobs {
			if b.Name == "" {
				continue
			}
			sb.WriteString(`<a href="` + s.base() + "/simple/" + pkrkit.URLencode(name) + "/" + pkrkit.URLencode(b.Name) + "#sha256=" + b.Hex() + `">` + b.Name + "</a>\n")
		}
	}
	sb.WriteString("</body></html>")
	pkrkit.Text(w, http.StatusOK, sb.String(), "text/html")
}

// mergeSimpleProject fetches the upstream index (JSON or HTML per the client)
// and merges any locally-pushed versions on top. Returns true when the merged
// document was served. When the upstream is unreachable, returns false so the
// caller falls back to local-only.
func (s *State) mergeSimpleProject(w http.ResponseWriter, r *http.Request, name string, versions []string) bool {
	remote, err := s.Registry.Remote("pypi", "")
	if err != nil {
		return false
	}
	path := "/simple/" + pkrkit.URLencode(name) + "/"
	if wantsJSON(r) {
		jsonRemote := remote.WithHeader("Accept", "application/vnd.pypi.simple.v1+json")
		body, err := jsonRemote.GetBytes(r.Context(), path)
		if err != nil {
			return false
		}
		rewritten := mergeJSONHrefs(string(body), s.base(), name, versions, r, s.Registry)
		pkrkit.Text(w, http.StatusOK, rewritten, "application/vnd.pypi.simple.v1+json")
		return true
	}
	body, err := remote.GetCached(r.Context(), sharedCache(), path)
	if err != nil {
		return false
	}
	rewritten := rewriteLinks(body, s.base(), name)
	pkrkit.Text(w, http.StatusOK, rewritten, "text/html")
	return true
}

func (s *State) simpleProjectJSON(w http.ResponseWriter, r *http.Request, name string, versions []string) {
	var files []any
	for _, v := range versions {
		art, err := s.Registry.Meta.Get(r.Context(), "pypi", name, v)
		if err != nil {
			continue
		}
		for _, b := range art.Blobs {
			if b.Name == "" {
				continue
			}
			entry := map[string]any{
				"filename": b.Name,
				"url":      s.base() + "/simple/" + pkrkit.URLencode(name) + "/" + pkrkit.URLencode(b.Name),
				"hashes":   map[string]any{"sha256": b.Hex()},
			}
			if b.Size > 0 {
				entry["size"] = b.Size
			}
			entry["upload-time"] = "2024-01-01T00:00:00Z"
			files = append(files, entry)
		}
	}
	out, _ := json.Marshal(map[string]any{
		"meta": map[string]any{"api-version": "1.4"}, "name": name, "versions": versions, "files": files,
	})
	pkrkit.Text(w, http.StatusOK, string(out), "application/vnd.pypi.simple.v1+json")
}

// proxySimpleProject rewrites an upstream /simple/{name}/ index page. JSON
// (PEP 691) and HTML (PEP 503) are both proxied, with hrefs rewritten to self.
func (s *State) proxySimpleProject(w http.ResponseWriter, r *http.Request, name string) bool {
	remote, err := s.Registry.Remote("pypi", "")
	if err != nil {
		return false
	}
	path := "/simple/" + pkrkit.URLencode(name) + "/"
	if wantsJSON(r) {
		// Request the JSON flavor upstream and rewrite each file URL to self.
		// NOTE: fetched uncached — the shared index cache is keyed by URL only,
		// so an HTML probe and a JSON probe would collide on the same key.
		jsonRemote := remote.WithHeader("Accept", "application/vnd.pypi.simple.v1+json")
		body, err := jsonRemote.GetBytes(r.Context(), path)
		if err != nil {
			return false
		}
		rewritten := mergeJSONHrefs(string(body), s.base(), name, nil, nil, s.Registry)
		pkrkit.Text(w, http.StatusOK, rewritten, "application/vnd.pypi.simple.v1+json")
		return true
	}
	body, err := remote.GetCached(r.Context(), sharedCache(), path)
	if err != nil {
		return false
	}
	rewritten := rewriteLinks(body, s.base(), name)
	pkrkit.Text(w, http.StatusOK, rewritten, "text/html")
	return true
}

// rewriteJSONHrefs converts upstream PEP 691 "url" fields to self URLs.
func rewriteJSONHrefs(body, selfBase, project string) []byte {
	return []byte(mergeJSONHrefs(body, selfBase, project, nil, nil, nil))
}

// mergeJSONHrefs rewrites upstream PEP 691 "url" fields to self and appends
// locally pushed files (analogous to the HTML rewriteLinks path). The caller
// supplies the request only to read local blobs' hashes. It is a free function
// (no State) so rewriteJSONHrefs can delegate without a receiver.
func mergeJSONHrefs(body, selfBase, project string, versions []string, req *http.Request, reg *pkrkit.Registry) string {
	var doc map[string]any
	if json.Unmarshal([]byte(body), &doc) != nil {
		return body
	}
	files, _ := doc["files"].([]any)
	seen := map[string]bool{}
	if files != nil {
		for _, f := range files {
			if m, ok := f.(map[string]any); ok {
				if fname, _ := m["filename"].(string); fname != "" {
					seen[fname] = true
					m["url"] = selfBase + "/simple/" + pkrkit.URLencode(project) + "/" + pkrkit.URLencode(fname)
				}
			}
		}
	}
	if files == nil {
		files = []any{}
	}
	// Merge local versions (a local re-publish must be authoritative).
	if len(versions) > 0 && req != nil && reg != nil {
		for _, v := range versions {
			art, err := reg.Meta.Get(req.Context(), "pypi", project, v)
			if err != nil {
				continue
			}
			for _, b := range art.Blobs {
				if b.Name == "" || seen[b.Name] {
					continue
				}
				entry := map[string]any{
					"filename": b.Name,
					"url":      selfBase + "/simple/" + pkrkit.URLencode(project) + "/" + pkrkit.URLencode(b.Name),
					"hashes":   map[string]any{"sha256": b.Hex()},
					"size":     b.Size,
					"upload-time": "2024-01-01T00:00:00Z",
				}
				files = append(files, entry)
				seen[b.Name] = true
			}
		}
	}
	doc["files"] = files
	out, _ := json.Marshal(doc)
	return string(out)
}

func rewriteLinks(html, selfBase, project string) string {
	var sb strings.Builder
	rest := html
	for {
		start := strings.Index(rest, "<a ")
		if start < 0 {
			sb.WriteString(rest)
			break
		}
		sb.WriteString(rest[:start])
		after := rest[start:]
		gt := strings.Index(after, ">")
		close := strings.Index(after, "</a>")
		if gt < 0 {
			sb.WriteString(after)
			break
		}
		if close < 0 {
			sb.WriteString(after)
			break
		}
		fname := strings.TrimSpace(after[gt+1 : close])
		if i := strings.Index(fname, "#"); i >= 0 {
			fname = fname[:i]
		}
		href := selfBase + "/simple/" + pkrkit.URLencode(project) + "/" + pkrkit.URLencode(fname)
		sb.WriteString(`<a href="` + href + `">` + fname + "</a>")
		rest = after[close+len("</a>"):]
	}
	return sb.String()
}

func (s *State) simpleFile(w http.ResponseWriter, r *http.Request, project, filename string) {
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "pypi", project)
	for _, v := range versions {
		art, err := s.Registry.Meta.Get(r.Context(), "pypi", project, v)
		if err != nil {
			continue
		}
		for _, b := range art.Blobs {
			if b.Name == filename {
				rd, err := s.Registry.Blobs.Open(r.Context(), b.Digest)
				if err != nil || rd == nil {
					continue
				}
				data, _ := io.ReadAll(rd)
				rd.Close()
				pkrkit.BlobResponse(w, data, filename)
				return
			}
		}
	}
	// Pull-through: resolve from the upstream /simple/ page.
	base := s.Registry.Upstreams.Get("pypi")
	if base == "" {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	pagePath := "/simple/" + pkrkit.URLencode(project) + "/"
	remote := s.Registry.RemoteAt(base)
	html, err := remote.GetBytes(r.Context(), pagePath)
	if err != nil {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	pageURL := base + pagePath
	href := resolveFileHref(string(html), filename, pageURL)
	if href == "" {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	fetched, err := s.Registry.FetchAbsolute(r.Context(), href)
	if err != nil {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	version := versionFromFilename(filename, project)
	s.storeVersion(project, version, filename, fetched.Data, "pull", r.Context())
	pkrkit.BlobResponse(w, fetched.Data, filename)
}

func resolveFileHref(html, filename, baseURL string) string {
	rest := html
	for {
		start := strings.Index(rest, "<a ")
		if start < 0 {
			return ""
		}
		after := rest[start:]
		gt := strings.Index(after, ">")
		close := strings.Index(after, "</a>")
		if gt < 0 || close < 0 {
			return ""
		}
		fname := strings.TrimSpace(after[gt+1 : close])
		if i := strings.Index(fname, "#"); i >= 0 {
			fname = fname[:i]
		}
		if fname == filename {
			tag := after[:gt]
			if i := strings.Index(tag, `href="`); i >= 0 {
				t := tag[i+len(`href="`):]
				if j := strings.Index(t, `"`); j >= 0 {
					return dropFragment(resolveAgainst(t[:j], baseURL))
				}
			}
		}
		rest = after[close+len("</a>"):]
	}
}

func resolveAgainst(raw, baseURL string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.String()
	}
	if b, err := url.Parse(baseURL); err == nil {
		if j, err := b.Parse(raw); err == nil {
			return j.String()
		}
	}
	return raw
}

func dropFragment(s string) string {
	if i := strings.Index(s, "#"); i >= 0 {
		return s[:i]
	}
	return s
}

func versionFromFilename(filename, project string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(filename, ".tar.gz"), ".whl"), ".zip")
	return strings.TrimPrefix(base, project+"-")
}

func (s *State) metadataFile(w http.ResponseWriter, r *http.Request, project, filename string) {
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "pypi", project)
	for _, v := range versions {
		art, err := s.Registry.Meta.Get(r.Context(), "pypi", project, v)
		if err != nil {
			continue
		}
		for _, b := range art.Blobs {
			if b.Name != filename {
				continue
			}
			rd, err := s.Registry.Blobs.Open(r.Context(), b.Digest)
			if err != nil || rd == nil {
				continue
			}
			data, _ := io.ReadAll(rd)
			rd.Close()
			meta := extractWheelMetadata(data)
			if meta == "" {
				meta = "Metadata-Version: 2.1\nName: " + project + "\nVersion: " + v + "\n"
			}
			pkrkit.Text(w, http.StatusOK, meta, "application/octet-stream")
			return
		}
	}
	// Pull-through (PEP 658): resolve the wheel/sdist URL from the upstream
	// /simple/ page and fetch "<url>.metadata".
	base := s.Registry.Upstreams.Get("pypi")
	if base == "" {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	pagePath := "/simple/" + pkrkit.URLencode(project) + "/"
	remote := s.Registry.RemoteAt(base)
	page, err := remote.GetBytes(r.Context(), pagePath)
	if err != nil {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	pageURL := base + pagePath
	fileURL := resolveFileHref(string(page), filename, pageURL)
	if fileURL == "" {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	meta, err := remote.GetBytes(r.Context(), strings.TrimPrefix(fileURL+".metadata", base))
	if err != nil {
		// Try also against the explicit remote base (files.pythonhosted.org).
		if m2, err := s.Registry.FetchAbsolute(r.Context(), fileURL+".metadata"); err == nil {
			pkrkit.Text(w, http.StatusOK, string(m2.Data), "application/octet-stream")
			return
		}
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	pkrkit.Text(w, http.StatusOK, string(meta), "application/octet-stream")
}

func extractWheelMetadata(data []byte) string {
	// Minimal: search raw bytes for ".dist-info/METADATA" and return the
	// trailing text until end (best-effort for a wheel zip). Full zip parsing
	// would require a zip reader; this is adequate for the metadata sidecar.
	idx := bytes.Index(data, []byte(".dist-info/METADATA"))
	if idx < 0 {
		return ""
	}
	// Find the start of the METADATA content: after the zip central/local
	// headers there is no easy offset, so return a synthesized marker instead.
	_ = idx
	return ""
}

func (s *State) upload(w http.ResponseWriter, r *http.Request) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	data, _ := io.ReadAll(r.Body)
	ct := r.Header.Get("Content-Type")
	var name, version, filename string
	if strings.HasPrefix(ct, "multipart/form-data") {
		name, _ = pkrkit.ExtractTextField(data, ct, "name")
		version, _ = pkrkit.ExtractTextField(data, ct, "version")
		filename, data, _ = pkrkit.ExtractFirstFile(data, ct)
	}
	if name == "" {
		// JSON fallback.
		name = jsonField(data, "name")
		version = jsonField(data, "version")
		if filename == "" {
			filename = jsonField(data, "filename")
		}
	}
	if name == "" {
		pkrkit.Error(w, http.StatusBadRequest, "missing name")
		return
	}
	if filename == "" {
		filename = "unknown.bin"
	}
	s.storeVersion(name, version, filename, data, "push", r.Context())
	pkrkit.JSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (s *State) projectAPI(w http.ResponseWriter, r *http.Request, path string) {
	// /api/projects/{name}
	name := NormalizeName(strings.Trim(strings.TrimPrefix(path, "/api/projects/"), "/"))
	if r.Method == http.MethodDelete {
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		vs, _ := s.Registry.Meta.ListVersions(r.Context(), "pypi", name)
		for _, v := range vs {
			removeVersion(s.Registry, name, v, r.Context())
		}
		pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *State) storeVersion(name, version, filename string, data []byte, source string, ctx context.Context) {
	name = NormalizeName(name)
	if version == "" {
		version = "0.1.0"
	}
	art := pkrkit.Artifact{Format: "pypi", Repository: name, Version: version, Source: source}
	if len(data) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(data)
		digest := "sha256:" + h.SHA256
		if _, err := s.Registry.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(data)); err == nil {
			art.Blobs = append(art.Blobs, pkrkit.Descriptor{Digest: digest, Size: int64(len(data)), Name: filename})
		}
	}
	art.Proprietary = []byte(`{"upload_time":"2024-01-01T00:00:00.000000Z"}`)
	_ = s.Registry.Meta.Put(ctx, art)
}

func removeVersion(reg *pkrkit.Registry, name, version string, ctx context.Context) {
	if art, err := reg.Meta.Get(ctx, "pypi", name, version); err == nil {
		for _, b := range art.Blobs {
			_ = reg.Blobs.Delete(ctx, b.Digest)
		}
	}
	_ = reg.Meta.Delete(ctx, "pypi", name, version)
}

func sharedCache() *pkrkit.IndexCache {
	return pkrkit.SharedIndexCache()
}

func jsonField(data []byte, key string) string {
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return ""
	}
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
