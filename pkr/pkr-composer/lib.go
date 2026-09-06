// Package composer implements the Composer repository protocol: packages.json,
// p2 metadata, dist download with pull-through, upload, search, list. Mirror
// of the pkglab reference.
package composer

import (
	"archive/zip"
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

func init() { pkrkit.Register("composer", NewHandler) }

func (s *State) base() string {
	if s.SelfBase == "" {
		return "http://localhost:8080/pkgs/composer"
	}
	return strings.TrimSuffix(s.SelfBase, "/")
}

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/composer")
	path = strings.TrimPrefix(path, "/composer")
	path = strings.Trim(path, "/")

	switch {
	case path == "packages.json":
		s.packagesJSON(w, r)
	case path == "list.json":
		s.listJSON(w, r)
	case path == "search.json":
		s.search(w, r)
	case strings.HasPrefix(path, "p2/"):
		s.p2(w, r, strings.TrimPrefix(path, "p2/"))
	case strings.HasPrefix(path, "p/") && !strings.HasPrefix(path, "providers/"):
		s.provider(w, r, strings.TrimPrefix(path, "p/"))
	case strings.HasPrefix(path, "providers/"):
		s.providersAPI(w, r, strings.TrimPrefix(path, "providers/"))
	case strings.HasPrefix(path, "dist/"):
		s.dist(w, r, strings.TrimPrefix(path, "dist/"))
	case path == "downloads" || path == "downloads/":
		w.WriteHeader(http.StatusOK)
	case path == "api/packages":
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			s.upload(w, r)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	case strings.HasPrefix(path, "api/packages/"):
		if r.Method == http.MethodDelete {
			s.deletePackage(w, r, strings.TrimPrefix(path, "api/packages/"))
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *State) packagesJSON(w http.ResponseWriter, r *http.Request) {
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "composer")
	base := s.base()
	providers := map[string]any{}
	for _, name := range repos {
		providers[name] = map[string]any{"sha256": nil}
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{
		"packages": []any{}, "metadata-url": base + "/p2/%package%.json",
		"available-packages": repos, "providers-url": base + "/p/%package%$%hash%.json",
		"providers": providers, "providers-api": base + "/providers/%package%.json",
		"list": base + "/list.json", "search": base + "/search.json?q=%query%&type=%type%",
		"provider-includes": map[string]any{}, "notify-batch": base + "/downloads/",
	})
}

func (s *State) listJSON(w http.ResponseWriter, r *http.Request) {
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "composer")
	filter := r.URL.Query().Get("filter")
	filter = strings.ReplaceAll(filter, "%2A", "*")
	var names []string
	if filter == "" {
		names = repos
	} else {
		for _, n := range repos {
			if globMatch(n, filter) {
				names = append(names, n)
			}
		}
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"packageNames": names})
}

func (s *State) search(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "composer")
	var results []any
	for _, n := range repos {
		if q != "" && !strings.Contains(strings.ToLower(n), q) {
			continue
		}
		results = append(results, map[string]any{"name": n, "description": "", "url": "", "downloads": 0})
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"results": results, "total": len(results)})
}

func (s *State) p2(w http.ResponseWriter, r *http.Request, rest string) {
	rest = strings.TrimSuffix(rest, ".json")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	full := strings.Join(parts[:2], "/")
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "composer", full)
	if len(versions) == 0 {
		remote, err := s.Registry.Remote("composer", "")
		if err == nil {
			if body, err := remote.GetCached(r.Context(), pkrkit.SharedIndexCache(), "/p2/"+pkrkit.URLencode(full)+".json"); err == nil {
				pkrkit.JSON(w, http.StatusOK, json.RawMessage(body))
				return
			}
		}
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	var vs []any
	for _, v := range versions {
		vs = append(vs, s.versionEntry(r.Context(), full, v))
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"packages": map[string]any{full: vs}})
}

func (s *State) versionEntry(ctx context.Context, name, version string) map[string]any {
	base := s.base()
	entry := map[string]any{
		"name": name, "version": version, "version_normalized": normalizeVersion(version),
		"type": "library", "uid": version,
		"dist":    map[string]any{"type": "zip", "url": base + "/dist/" + name + "/" + pkrkit.URLencode(version) + "/" + version + ".zip", "shasum": ""},
		"require": map[string]any{"php": ">=7.4"},
	}
	if art, err := s.Registry.Meta.Get(ctx, "composer", name, version); err == nil && len(art.Proprietary) > 0 {
		var m map[string]any
		if json.Unmarshal(art.Proprietary, &m) == nil {
			if al, ok := m["autoload"]; ok {
				entry["autoload"] = al
			}
		}
	}
	return entry
}

func normalizeVersion(v string) string {
	parts := strings.Split(v, ".")
	for len(parts) < 4 {
		parts = append(parts, "0")
	}
	return strings.Join(parts, ".")
}

func (s *State) provider(w http.ResponseWriter, r *http.Request, rest string) {
	rest = strings.TrimSuffix(rest, ".json")
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	full := strings.Join(parts[:2], "/")
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "composer", full)
	if len(versions) == 0 {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	var vs []any
	for _, v := range versions {
		vs = append(vs, s.versionEntry(r.Context(), full, v))
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"packages": map[string]any{full: vs}})
}

func (s *State) providersAPI(w http.ResponseWriter, r *http.Request, rest string) {
	_ = rest
	pkrkit.JSON(w, http.StatusOK, map[string]any{"providers": map[string]any{}})
}

func (s *State) dist(w http.ResponseWriter, r *http.Request, rest string) {
	rep := strings.Trim(rest, "/")
	parts := strings.Split(rep, "/")
	if len(parts) < 3 {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	version, ref := parts[len(parts)-2], parts[len(parts)-1]
	full := strings.Join(parts[:len(parts)-2], "/")
	short := full[strings.LastIndex(full, "/")+1:]
	filename := short + "-" + version + ".zip"
	if art, err := s.Registry.Meta.Get(r.Context(), "composer", full, version); err == nil {
		for _, b := range art.Blobs {
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
	fetched, err := s.Registry.Fetch(r.Context(), "composer", "", "/dist/"+full+"/"+version+"/"+ref)
	if err != nil {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	storeVersionSource(s.Registry, full, version, fetched.Data, "pull", r.Context())
	pkrkit.BlobResponse(w, fetched.Data, filename)
}

func (s *State) upload(w http.ResponseWriter, r *http.Request) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	data, _ := io.ReadAll(r.Body)
	name, version := composerJSONNameVersion(data)
	if name == "" {
		name = "vendor/pkg"
	}
	if version == "" {
		version = "0.1.0"
	}
	storeVersionSource(s.Registry, name, version, data, "push", r.Context())
	pkrkit.JSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (s *State) deletePackage(w http.ResponseWriter, r *http.Request, rest string) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	full := strings.Trim(rest, "/")
	full = strings.ReplaceAll(full, "/", "/")
	vs, _ := s.Registry.Meta.ListVersions(r.Context(), "composer", full)
	for _, v := range vs {
		removeVersion(s.Registry, full, v, r.Context())
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func removeVersion(reg *pkrkit.Registry, name, version string, ctx context.Context) {
	if art, err := reg.Meta.Get(ctx, "composer", name, version); err == nil {
		for _, b := range art.Blobs {
			_ = reg.Blobs.Delete(ctx, b.Digest)
		}
	}
	_ = reg.Meta.Delete(ctx, "composer", name, version)
}

func storeVersionSource(reg *pkrkit.Registry, name, version string, data []byte, source string, ctx context.Context) {
	art := pkrkit.Artifact{Format: "composer", Repository: name, Version: version, Source: source}
	if len(data) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(data)
		digest := "sha256:" + h.SHA256
		short := name[strings.LastIndex(name, "/")+1:]
		if _, err := reg.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(data)); err == nil {
			art.Blobs = append(art.Blobs, pkrkit.Descriptor{Digest: digest, Size: int64(len(data)), Name: short + "-" + version + ".zip"})
		}
		if al := extractAutoload(data); al != nil {
			art.Proprietary, _ = json.Marshal(map[string]any{"autoload": al})
		}
	}
	_ = reg.Meta.Put(ctx, art)
}

func composerJSONNameVersion(data []byte) (string, string) {
	b, ok := composerJSONBytes(data)
	if !ok {
		return "", ""
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return "", ""
	}
	name, _ := m["name"].(string)
	version, _ := m["version"].(string)
	return name, version
}

func composerJSONBytes(data []byte) ([]byte, bool) {
	if len(data) >= 2 && data[0] == 'P' && data[1] == 'K' {
		zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return data, false
		}
		for _, f := range zr.File {
			if f.Name == "composer.json" {
				rc, _ := f.Open()
				buf, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
				rc.Close()
				return buf, true
			}
		}
		return data, false
	}
	return data, json.Valid(data)
}

func extractAutoload(data []byte) map[string]any {
	b, ok := composerJSONBytes(data)
	if !ok {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return nil
	}
	al, _ := m["autoload"].(map[string]any)
	return al
}

func globMatch(name, pattern string) bool {
	// Convert a simple glob (*, ?) to matched via regex-lite.
	re := "^"
	for _, c := range pattern {
		switch c {
		case '*':
			re += ".*"
		case '?':
			re += "."
		default:
			re += strings.ReplaceAll(string(c), ".", `\.`)
		}
	}
	re += "$"
	matched, _ := regexp.MatchString(re, name)
	return matched
}
