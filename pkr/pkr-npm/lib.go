// Package npm implements the npm registry protocol: packument assembly
// (dist.tarball rewrite, shasum sha1, integrity sha512 base64), tarball
// serving, publish (raw tarball or CouchDB _attachments base64), dist-tags,
// deprecate, -rev unpublish, search, audit/profile stubs, pull-through with
// packument URL rewriting. Mirror of the pkglab reference.
package npm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
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

func init() { pkrkit.Register("npm", NewHandler) }

func (s *State) base() string {
	if s.SelfBase == "" {
		return "http://localhost:8080/pkgs/npm"
	}
	return strings.TrimSuffix(s.SelfBase, "/")
}

func tarballFilename(name, version string) string {
	short := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		short = name[i+1:]
	}
	return short + "-" + version + ".tgz"
}

func unescapeName(name string) string {
	if u, err := url.QueryUnescape(name); err == nil {
		return u
	}
	return name
}

func encodeName(name string) string { return strings.ReplaceAll(name, "/", "%2F") }

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/npm")
	path = strings.TrimPrefix(path, "/npm")
	path = strings.Trim(path, "/")
	method := r.Method

	switch {
	case path == "":
		pkrkit.JSON(w, http.StatusOK, map[string]any{"db_name": "registry", "doc_count": 0})
	case path == "-/ping":
		pkrkit.JSON(w, http.StatusOK, map[string]any{})
	case path == "-/whoami":
		pkrkit.JSON(w, http.StatusOK, map[string]any{"username": "anonymous"})
	case path == "-/all":
		s.allPackages(w, r)
	case path == "-/v1/login":
		pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true, "token": "npm-anonymous"})
	case strings.HasPrefix(path, "-/npm/v1/security"):
		s.securityStub(w, r, path)
	case path == "-/npm/v1/user":
		pkrkit.JSON(w, http.StatusOK, map[string]any{"name": "anonymous", "email": "", "email_verified": false, "tfa": nil})
	case strings.HasPrefix(path, "-/npm/v1/tokens"):
		s.tokens(w, r, method)
	case strings.HasPrefix(path, "-/user/"):
		s.couchUser(w, r, path, method)
	case strings.HasPrefix(path, "-/v1/search"):
		s.search(w, r)
	case strings.HasPrefix(path, "-/package/"):
		s.distTags(w, r, path, method)
	case strings.Contains(path, "/-rev/"):
		s.rev(w, r, path, method)
	case method == http.MethodPut && strings.HasSuffix(path, "/deprecate"):
		name := strings.TrimSuffix(path, "/deprecate")
		s.deprecate(w, r, name)
	case method == http.MethodPut:
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		s.publish(w, r, unescapeName(path))
	case method == http.MethodDelete:
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		s.deleteName(w, r, unescapeName(path))
	default:
		if i := strings.Index(path, "/-/"); i >= 0 {
			pkg := unescapeName(path[:i])
			file := path[i+len("/-/"):]
			if file == "package" || strings.HasPrefix(file, "package/") {
				s.distTags(w, r, path, method)
				return
			}
			if method == http.MethodGet {
				s.tarball(w, r, pkg, file)
				return
			}
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if method == http.MethodGet {
			s.metadata(w, r, unescapeName(path))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *State) allPackages(w http.ResponseWriter, r *http.Request) {
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "npm")
	entries := map[string]any{}
	for _, repo := range repos {
		versions, _ := s.Registry.Meta.ListVersions(r.Context(), "npm", repo)
		latest := pkrkit.HighestVersion(versions)
		if latest == "" && len(versions) > 0 {
			latest = versions[len(versions)-1]
		}
		entries[repo] = map[string]any{"name": repo, "latest": latest}
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"db_name": "registry", "_updated": 0, "entries": entries})
}

func (s *State) securityStub(w http.ResponseWriter, r *http.Request, path string) {
	if strings.Contains(path, "/quick") {
		pkrkit.JSON(w, http.StatusOK, map[string]any{"secret": "audits-quick"})
		return
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{})
}

func (s *State) tokens(w http.ResponseWriter, r *http.Request, method string) {
	switch method {
	case http.MethodDelete:
		pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodPost:
		pkrkit.JSON(w, http.StatusCreated, map[string]any{"token": "npm-anonymous", "key": "npm-anonymous"})
	default:
		pkrkit.JSON(w, http.StatusOK, map[string]any{"objects": []any{}, "total": 0})
	}
}

func (s *State) couchUser(w http.ResponseWriter, r *http.Request, path, method string) {
	name := strings.TrimPrefix(path, "-/user/")
	// Strip "org.couchdb.user:" prefix if present.
	name = name[strings.LastIndex(name, ":")+1:]
	switch method {
	case http.MethodPut, http.MethodPost:
		pkrkit.JSON(w, http.StatusCreated, map[string]any{"ok": true, "name": name, "token": "npm-anonymous", "id": "org.couchdb.user:" + name, "rev": "1"})
	case http.MethodDelete:
		pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		pkrkit.JSON(w, http.StatusOK, map[string]any{"_id": path, "name": path})
	}
}

func (s *State) search(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(r.URL.Query().Get("text"), "%20", " "), "+", " "))
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "npm")
	var objects []any
	for _, repo := range repos {
		if q != "" && !strings.Contains(strings.ToLower(repo), q) {
			continue
		}
		versions, _ := s.Registry.Meta.ListVersions(r.Context(), "npm", repo)
		latest := pkrkit.HighestVersion(versions)
		if latest == "" && len(versions) > 0 {
			latest = versions[len(versions)-1]
		}
		objects = append(objects, map[string]any{"package": map[string]any{"name": repo, "version": latest}})
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"objects": objects, "total": len(objects), "time": ""})
}

func (s *State) distTags(w http.ResponseWriter, r *http.Request, rel, method string) {
	rest := strings.TrimSuffix(rel, "/")
	if !strings.HasPrefix(rest, "-/package/") {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	rest = strings.TrimPrefix(rest, "-/package/")
	idx := strings.Index(rest, "/dist-tags")
	if idx < 0 {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	pkg := unescapeName(rest[:idx])
	tag := strings.Trim(strings.TrimPrefix(rest[idx+len("/dist-tags"):], "/"), "/")

	switch method {
	case http.MethodGet:
		dt := s.distTagsMap(r.Context(), pkg)
		if tag != "" {
			if v, ok := dt[tag]; ok {
				pkrkit.JSON(w, http.StatusOK, v)
				return
			}
			pkrkit.Error(w, http.StatusNotFound, "not found")
			return
		}
		pkrkit.JSON(w, http.StatusOK, dt)
	case http.MethodPut:
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		body, _ := io.ReadAll(r.Body)
		version := strings.Trim(strings.TrimSpace(string(body)), `"`)
		if version == "" {
			pkrkit.Error(w, http.StatusNotFound, "missing version")
			return
		}
		dt := s.distTagsMap(r.Context(), pkg)
		if _, err := s.Registry.Meta.Get(r.Context(), "npm", pkg, version); err != nil {
			pkrkit.Error(w, http.StatusNotFound, "version "+version+" not found")
			return
		}
		dt[tag] = version
		s.saveDistTags(r.Context(), pkg, dt)
		pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodDelete:
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		dt := s.distTagsMap(r.Context(), pkg)
		delete(dt, tag)
		s.saveDistTags(r.Context(), pkg, dt)
		pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *State) distTagsMap(ctx context.Context, pkg string) map[string]any {
	m := map[string]any{}
	versions, _ := s.Registry.Meta.ListVersions(ctx, "npm", pkg)
	latest := pkrkit.HighestVersion(versions)
	if latest == "" && len(versions) > 0 {
		latest = versions[len(versions)-1]
	}
	if latest != "" {
		m["latest"] = latest
	}
	if art, err := s.Registry.Meta.Get(ctx, "npm", pkg, ""); err == nil && len(art.Proprietary) > 0 {
		var root map[string]any
		if json.Unmarshal(art.Proprietary, &root) == nil {
			if dt, ok := root["dist-tags"].(map[string]any); ok && len(dt) > 0 {
				return dt
			}
		}
	}
	return m
}

func (s *State) saveDistTags(ctx context.Context, pkg string, dt map[string]any) {
	root := map[string]any{"dist-tags": dt}
	b, _ := json.Marshal(root)
	_ = s.Registry.Meta.Put(ctx, pkrkit.Artifact{Format: "npm", Repository: pkg, Version: "", Proprietary: b})
}

func (s *State) deprecate(w http.ResponseWriter, r *http.Request, name string) {
	name = unescapeName(name)
	body, _ := io.ReadAll(r.Body)
	var doc map[string]any
	if json.Unmarshal(body, &doc) == nil {
		if versions, ok := doc["versions"].(map[string]any); ok {
			for ver, msg := range versions {
				art, err := s.Registry.Meta.Get(r.Context(), "npm", name, ver)
				if err != nil {
					continue
				}
				pj := map[string]any{}
				_ = json.Unmarshal(art.Proprietary, &pj)
				switch v := msg.(type) {
				case string:
					if v == "" {
						delete(pj, "deprecated")
					} else {
						pj["deprecated"] = v
					}
				default:
					delete(pj, "deprecated")
				}
				art.Proprietary, _ = json.Marshal(pj)
				_ = s.Registry.Meta.Put(r.Context(), art)
			}
		}
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *State) rev(w http.ResponseWriter, r *http.Request, rel, method string) {
	i := strings.Index(rel, "/-rev/")
	if i < 0 {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	pkg := unescapeName(rel[:i])
	switch method {
	case http.MethodDelete:
		vs, _ := s.Registry.Meta.ListVersions(r.Context(), "npm", pkg)
		for _, v := range vs {
			s.removeVersion(r.Context(), pkg, v)
		}
		_ = s.Registry.Meta.Delete(r.Context(), "npm", pkg, "")
		pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
	case http.MethodPut:
		body, _ := io.ReadAll(r.Body)
		var doc map[string]any
		if json.Unmarshal(body, &doc) == nil {
			if name, ok := doc["name"].(string); ok {
				s.reconcileVersions(r.Context(), name, doc)
			}
		}
		pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		pkrkit.Error(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *State) reconcileVersions(ctx context.Context, pkg string, doc map[string]any) {
	keep := map[string]bool{}
	if vs, ok := doc["versions"].(map[string]any); ok {
		for k := range vs {
			keep[k] = true
		}
	}
	current, _ := s.Registry.Meta.ListVersions(ctx, "npm", pkg)
	for _, v := range current {
		if !keep[v] {
			s.removeVersion(ctx, pkg, v)
		}
	}
}

func (s *State) deleteName(w http.ResponseWriter, r *http.Request, name string) {
	if i := strings.LastIndex(name, "/"); i > 0 {
		s.removeVersion(r.Context(), name[:i], name[i+1:])
		pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	vs, _ := s.Registry.Meta.ListVersions(r.Context(), "npm", name)
	for _, v := range vs {
		s.removeVersion(r.Context(), name, v)
	}
	_ = s.Registry.Meta.Delete(r.Context(), "npm", name, "")
	pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *State) removeVersion(ctx context.Context, pkg, version string) {
	if art, err := s.Registry.Meta.Get(ctx, "npm", pkg, version); err == nil {
		for _, b := range art.Blobs {
			_ = s.Registry.Blobs.Delete(ctx, b.Digest)
		}
	}
	_ = s.Registry.Meta.Delete(ctx, "npm", pkg, version)
}

func (s *State) metadata(w http.ResponseWriter, r *http.Request, name string) {
	if body := s.aggregateMetadata(r.Context(), name); body != "" {
		pkrkit.JSON(w, http.StatusOK, json.RawMessage(body))
		return
	}
	remote, err := s.Registry.Remote("npm", "")
	if err != nil {
		pkrkit.Error(w, http.StatusNotFound, "package not found")
		return
	}
	body, err := remote.GetCached(r.Context(), pkrkit.SharedIndexCache(), "/"+encodeName(name))
	if err != nil {
		pkrkit.Error(w, http.StatusNotFound, "package not found")
		return
	}
	pkrkit.JSON(w, http.StatusOK, json.RawMessage(s.rewriteTarballURLs(body, name)))
}

func (s *State) aggregateMetadata(ctx context.Context, name string) string {
	versions, _ := s.Registry.Meta.ListVersions(ctx, "npm", name)
	if len(versions) == 0 {
		return ""
	}
	vmap := map[string]any{}
	// Local versions are authoritative; but the resolver still needs the FULL
	// version list to satisfy ranges (e.g. is-even -> is-odd@^0.1.2 while only
	// 3.0.1 is cached). Overlay upstream versions (tarballs rewritten to self)
	// beneath them, then re-apply local metadata over the same version.
	if remote, err := s.Registry.Remote("npm", ""); err == nil {
		if body, err := remote.GetCached(ctx, pkrkit.SharedIndexCache(), "/"+encodeName(name)); err == nil {
			var up map[string]any
			if json.Unmarshal([]byte(body), &up) == nil {
				if uv, ok := up["versions"].(map[string]any); ok {
					for ver, info := range uv {
						if _, exists := vmap[ver]; exists {
							continue
						}
						if m, ok := info.(map[string]any); ok {
							if dist, ok := m["dist"].(map[string]any); ok {
								dist["tarball"] = s.tarballURL(name, ver)
							}
							vmap[ver] = m
						}
					}
				}
			}
		}
	}
	// Re-apply local metadata (authoritative) over everything.
	for _, v := range versions {
		art, err := s.Registry.Meta.Get(ctx, "npm", name, v)
		if err != nil {
			continue
		}
		pj := map[string]any{}
		_ = json.Unmarshal(art.Proprietary, &pj)
		if _, ok := pj["name"]; !ok {
			pj["name"] = name
		}
		if _, ok := pj["version"]; !ok {
			pj["version"] = v
		}
		dist := map[string]any{"tarball": s.tarballURL(name, v)}
		for _, b := range art.Blobs {
			if b.Digest == "" {
				continue
			}
			h, err := s.Registry.Blobs.HashesFor(ctx, b.Digest)
			if err == nil {
				dist["shasum"] = h.SHA1
				intB64 := base64.StdEncoding.EncodeToString(hexBytes(h.SHA512))
				dist["integrity"] = "sha512-" + intB64
			}
			if b.Size > 0 {
				dist["size"] = b.Size
			}
		}
		pj["dist"] = dist
		vmap[v] = pj
	}
	latest := pkrkit.HighestVersion(versions)
	if latest == "" && len(versions) > 0 {
		latest = versions[len(versions)-1]
	}
	dt := s.distTagsMap(ctx, name)
	if _, ok := dt["latest"]; !ok {
		dt["latest"] = latest
	}
	doc := map[string]any{"_id": name, "_rev": "1", "name": name, "dist-tags": dt, "versions": vmap}
	b, _ := json.Marshal(doc)
	return string(b)
}

func (s *State) tarballURL(name, version string) string {
	return s.base() + "/" + encodeName(name) + "/-/" + tarballFilename(name, version)
}

func (s *State) rewriteTarballURLs(packument, name string) string {
	var doc map[string]any
	if json.Unmarshal([]byte(packument), &doc) != nil {
		return packument
	}
	if versions, ok := doc["versions"].(map[string]any); ok {
		for ver, info := range versions {
			if m, ok := info.(map[string]any); ok {
				if dist, ok := m["dist"].(map[string]any); ok {
					dist["tarball"] = s.tarballURL(name, ver)
				}
			}
		}
	}
	b, _ := json.Marshal(doc)
	return string(b)
}

func (s *State) tarball(w http.ResponseWriter, r *http.Request, name, file string) {
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "npm", name)
	for _, v := range versions {
		if tarballFilename(name, v) == file {
			if art, err := s.Registry.Meta.Get(r.Context(), "npm", name, v); err == nil {
				for _, b := range art.Blobs {
					rd, err := s.Registry.Blobs.Open(r.Context(), b.Digest)
					if err != nil || rd == nil {
						continue
					}
					data, _ := io.ReadAll(rd)
					rd.Close()
					pkrkit.BlobResponse(w, data, file)
					return
				}
			}
		}
	}
	remote, err := s.Registry.Remote("npm", "")
	if err != nil {
		pkrkit.Error(w, http.StatusNotFound, "tarball not found")
		return
	}
	data, err := remote.GetBytes(r.Context(), "/"+encodeName(name)+"/-/"+file)
	if err != nil {
		pkrkit.Error(w, http.StatusNotFound, "tarball not found")
		return
	}
	ver, pkgJSON := parseTarballPackageJSON(data)
	if ver == "" {
		ver = versionFromTarballName(file, name)
	}
	s.storeVersion(r.Context(), name, ver, data, pkgJSON, "pull")
	pkrkit.BlobResponse(w, data, file)
}

func versionFromTarballName(file, name string) string {
	short := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		short = name[i+1:]
	}
	prefix := short + "-"
	if strings.HasPrefix(file, prefix) {
		return strings.TrimSuffix(strings.TrimPrefix(file, prefix), ".tgz")
	}
	return ""
}

func parseTarballPackageJSON(data []byte) (string, map[string]any) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", nil
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if strings.HasSuffix(hdr.Name, "package.json") {
			buf, _ := io.ReadAll(io.LimitReader(tr, 1<<20))
			var m map[string]any
			if json.Unmarshal(buf, &m) == nil {
				ver, _ := m["version"].(string)
				return ver, m
			}
		}
	}
	return "", nil
}

func (s *State) publish(w http.ResponseWriter, r *http.Request, name string) {
	data, _ := io.ReadAll(r.Body)
	// Raw tarball (gzip magic).
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		ver, pkgJSON := parseTarballPackageJSON(data)
		pkgName := name
		if v, ok := pkgJSON["name"].(string); ok && v != "" {
			pkgName = v
		}
		s.storeVersion(r.Context(), pkgName, ver, data, pkgJSON, "push")
		pkrkit.JSON(w, http.StatusCreated, map[string]any{"ok": true})
		return
	}
	// CouchDB-style publish document.
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		pkrkit.Error(w, http.StatusBadRequest, "invalid payload")
		return
	}
	pkgName := name
	if v, ok := payload["name"].(string); ok && v != "" {
		pkgName = v
	}
	var version string
	var pkgJSON map[string]any
	if vs, ok := payload["versions"].(map[string]any); ok {
		for k, v := range vs {
			version = k
			pkgJSON, _ = v.(map[string]any)
			break
		}
	}
	if version == "" {
		if dt, ok := payload["dist-tags"].(map[string]any); ok {
			version, _ = dt["latest"].(string)
		}
	}
	if version == "" {
		pkrkit.Error(w, http.StatusBadRequest, "cannot determine version")
		return
	}
	var tarball []byte
	if atts, ok := payload["_attachments"].(map[string]any); ok {
		for _, v := range atts {
			if m, ok := v.(map[string]any); ok {
				if b64, ok := m["data"].(string); ok {
					if decoded, err := base64.StdEncoding.DecodeString(b64); err == nil {
						tarball = decoded
						break
					}
				}
			}
		}
	}
	s.storeVersion(r.Context(), pkgName, version, tarball, pkgJSON, "push")
	pkrkit.JSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (s *State) storeVersion(ctx context.Context, name, version string, tarball []byte, pkgJSON map[string]any, source string) {
	if name == "" {
		name = "unknown"
	}
	if version == "" {
		version = "0.0.0"
	}
	art := pkrkit.Artifact{Format: "npm", Repository: name, Version: version, MediaType: "application/json", Source: source}
	if pkgJSON != nil {
		art.Proprietary, _ = json.Marshal(pkgJSON)
	}
	if len(tarball) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(tarball)
		digest := "sha256:" + h.SHA256
		if _, err := s.Registry.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(tarball)); err == nil {
			art.Blobs = append(art.Blobs, pkrkit.Descriptor{Digest: digest, Size: int64(len(tarball)), Name: tarballFilename(name, version)})
		}
	}
	_ = s.Registry.Meta.Put(ctx, art)
}

func hexBytes(s string) []byte {
	if b, err := hex.DecodeString(s); err == nil {
		return b
	}
	return []byte(s)
}

var _ = sha1.New
var _ = sha512.New
