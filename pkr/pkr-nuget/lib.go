// Package nuget implements the NuGet v3 protocol: service index,
// search/autocomplete, registration index/leaf, flat container index + nupkg
// download (pull-through with URL rewriting), push, delete/relist. Mirror of
// the pkglab reference.
package nuget

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
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

func init() { pkrkit.Register("nuget", NewHandler) }

func (s *State) base() string {
	if s.SelfBase == "" {
		return "http://localhost:8080/pkgs/nuget"
	}
	return strings.TrimSuffix(s.SelfBase, "/")
}

func lower(s string) string      { return strings.ToLower(s) }
func isPrerelease(v string) bool { return strings.Contains(v, "-") }

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/nuget")
	path = strings.TrimPrefix(path, "/nuget")

	switch {
	case path == "/v3/index.json":
		s.serviceIndex(w, r)
	case path == "/v3/query":
		s.search(w, r)
	case path == "/v3/autocomplete":
		s.autocomplete(w, r)
	case strings.HasPrefix(path, "/v3/registration/") && strings.HasSuffix(path, "/index.json"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/v3/registration/"), "/index.json")
		s.registrationIndex(w, r, id)
	case strings.HasPrefix(path, "/v3/registration/"):
		rest := strings.TrimPrefix(path, "/v3/registration/")
		parts := strings.Split(rest, "/")
		if len(parts) == 2 {
			s.registrationVersion(w, r, parts[0], strings.TrimSuffix(parts[1], ".json"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case strings.HasPrefix(path, "/v3/flatcontainer/") && strings.HasSuffix(path, "/index.json"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/v3/flatcontainer/"), "/index.json")
		s.flatIndex(w, r, id)
	case strings.HasPrefix(path, "/v3/flatcontainer/"):
		rest := strings.TrimPrefix(path, "/v3/flatcontainer/")
		parts := strings.Split(rest, "/")
		if len(parts) == 3 {
			s.flatFile(w, r, parts[0], parts[1], parts[2])
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case (path == "/v3/package" || path == "/v3/package/" || path == "/api/v2/package" || path == "/api/v2/package/"):
		if r.Method == http.MethodPut {
			s.push(w, r)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	case strings.HasPrefix(path, "/v3/package/") || strings.HasPrefix(path, "/api/v2/package/"):
		rest := strings.TrimPrefix(strings.TrimPrefix(path, "/v3/package/"), "/api/v2/package/")
		parts := strings.Split(rest, "/")
		if len(parts) >= 2 {
			if r.Method == http.MethodDelete {
				s.deletePkg(w, r, parts[0], parts[1])
				return
			}
			if r.Method == http.MethodPut {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *State) serviceIndex(w http.ResponseWriter, r *http.Request) {
	base := s.base()
	pkrkit.JSON(w, http.StatusOK, map[string]any{
		"version": "3.0.0",
		"resources": []any{
			map[string]any{"@id": base + "/v3/query", "@type": "SearchQueryService/3.5.0"},
			map[string]any{"@id": base + "/v3/autocomplete", "@type": "SearchAutocompleteService/3.5.0"},
			map[string]any{"@id": base + "/v3/registration", "@type": "RegistrationsBaseUrl/3.6.0"},
			map[string]any{"@id": base + "/v3/flatcontainer/", "@type": "PackageBaseAddress/3.0.0"},
			map[string]any{"@id": base + "/v3/package", "@type": "PackagePublish/2.0.0"},
		},
	})
}

func (s *State) autocomplete(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "nuget")
	if id := r.URL.Query().Get("id"); id != "" {
		versions, _ := s.Registry.Meta.ListVersions(r.Context(), "nuget", id)
		total := len(versions)
		if r.URL.Query().Get("prerelease") != "true" {
			var v2 []string
			for _, v := range versions {
				if !isPrerelease(v) {
					v2 = append(v2, v)
				}
			}
			versions = v2
		}
		pkrkit.JSON(w, http.StatusOK, map[string]any{"totalHits": total, "data": versions})
		return
	}
	var data []string
	for _, name := range repos {
		if q == "" || strings.Contains(strings.ToLower(name), strings.ToLower(q)) {
			data = append(data, name)
		}
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"totalHits": len(data), "data": data})
}

func (s *State) search(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "nuget")
	base := s.base()
	var data []any
	for _, name := range repos {
		if q != "" && !strings.Contains(strings.ToLower(name), q) {
			continue
		}
		versions, _ := s.Registry.Meta.ListVersions(r.Context(), "nuget", name)
		latest := pkrkit.HighestVersion(versions)
		if latest == "" && len(versions) > 0 {
			latest = versions[len(versions)-1]
		}
		data = append(data, map[string]any{
			"id": name, "version": latest, "registration": base + "/v3/registration/" + lower(name) + "/index.json",
		})
	}
	if len(data) == 0 && q != "" {
		if base := s.Registry.Upstreams.Sub("nuget", "search"); base != "" {
			remote := s.Registry.RemoteAt(base)
			if body, err := remote.GetBytes(r.Context(), "/query?q="+pkrkit.URLencode(q)+"&prerelease=false"); err == nil {
				pkrkit.JSON(w, http.StatusOK, json.RawMessage(s.rewriteReg(body)))
				return
			}
		}
		pkrkit.Error(w, http.StatusBadGateway, "upstream")
		return
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"totalHits": len(data), "data": data})
}

func (s *State) registrationIndex(w http.ResponseWriter, r *http.Request, id string) {
	lid := lower(id)
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "nuget", id)
	if len(versions) == 0 {
		if base := s.Registry.Upstreams.Sub("nuget", "registration"); base != "" {
			remote := s.Registry.RemoteAt(base)
			if body, err := remote.GetCached(r.Context(), pkrkit.SharedIndexCache(), "/v3/registration5-semver1/"+lid+"/index.json"); err == nil {
				pkrkit.Text(w, http.StatusOK, s.rewriteReg([]byte(body)), "application/json")
				return
			}
		}
		pkrkit.Error(w, http.StatusNotFound, "package not found")
		return
	}
	base := s.base()
	var leaves []any
	for _, v := range versions {
		leaves = append(leaves, registrationLeaf(base, lid, id, v))
	}
	const pageSize = 64
	var items []any
	if len(leaves) <= pageSize {
		items = append(items, map[string]any{"@id": base + "/v3/registration/" + lid + "/index.json", "count": len(leaves), "items": leaves})
	} else {
		for i := 0; i < len(leaves); i += pageSize {
			end := i + pageSize
			if end > len(leaves) {
				end = len(leaves)
			}
			items = append(items, map[string]any{"@id": base + "/v3/registration/" + lid + "/page/" + itoa(i/pageSize) + ".json", "count": end - i, "items": leaves[i:end]})
		}
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"count": len(items), "items": items})
}

func registrationLeaf(base, lid, id, v string) map[string]any {
	return map[string]any{
		"@id":            base + "/v3/registration/" + lid + "/" + v + ".json",
		"catalogEntry":   map[string]any{"@id": base + "/v3/registration/" + lid + "/" + v + ".json", "id": id, "version": v},
		"packageContent": base + "/v3/flatcontainer/" + lid + "/" + v + "/" + lid + "." + v + ".nupkg",
	}
}

func (s *State) registrationVersion(w http.ResponseWriter, r *http.Request, id, ver string) {
	lid := lower(id)
	base := s.base()
	entry := map[string]any{
		"@id": base + "/v3/registration/" + lid + "/" + ver + ".json",
		"id":  id, "version": ver, "listed": true, "published": "2024-01-01T00:00:00Z",
		"packageContent": base + "/v3/flatcontainer/" + lid + "/" + ver + "/" + lid + "." + ver + ".nupkg",
		"catalogEntry":   map[string]any{"@id": base + "/v3/registration/" + lid + "/" + ver + ".json", "id": id, "version": ver, "listed": true, "published": "2024-01-01T00:00:00Z"},
	}
	pkrkit.JSON(w, http.StatusOK, entry)
}

func (s *State) rewriteReg(body []byte) string {
	out := string(body)
	out = strings.ReplaceAll(out, "https://api.nuget.org/v3-flatcontainer/", s.base()+"/v3/flatcontainer/")
	out = strings.ReplaceAll(out, "https://api.nuget.org/v3/registration5-semver1/", s.base()+"/v3/registration/")
	out = strings.ReplaceAll(out, "https://api.nuget.org/v3/registration5-gz-semver2/", s.base()+"/v3/registration/")
	out = strings.ReplaceAll(out, s.base()+"/v3/registration//", s.base()+"/v3/registration/")
	return out
}

func (s *State) flatIndex(w http.ResponseWriter, r *http.Request, id string) {
	lid := lower(id)
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "nuget", id)
	if len(versions) > 0 {
		pkrkit.JSON(w, http.StatusOK, map[string]any{"versions": versions})
		return
	}
	if base := s.Registry.Upstreams.Sub("nuget", "registration"); base != "" {
		remote := s.Registry.RemoteAt(base)
		if data, err := remote.GetBytes(r.Context(), "/v3-flatcontainer/"+lid+"/index.json"); err == nil {
			pkrkit.JSON(w, http.StatusOK, json.RawMessage(data))
			return
		}
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"versions": versions})
}

func (s *State) flatFile(w http.ResponseWriter, r *http.Request, id, ver, filename string) {
	if art, err := s.Registry.Meta.Get(r.Context(), "nuget", id, ver); err == nil {
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
	if base := s.Registry.Upstreams.Sub("nuget", "registration"); base != "" {
		remote := s.Registry.RemoteAt(base)
		lid := lower(id)
		if data, err := remote.GetBytes(r.Context(), "/v3-flatcontainer/"+lid+"/"+ver+"/"+filename); err == nil {
			storeVersionSource(s.Registry, id, ver, filename, data, "pull", r.Context())
			pkrkit.BlobResponse(w, data, filename)
			return
		}
	}
	pkrkit.Error(w, http.StatusNotFound, "not found")
}

func (s *State) deletePkg(w http.ResponseWriter, r *http.Request, id, ver string) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	if art, err := s.Registry.Meta.Get(r.Context(), "nuget", id, ver); err == nil {
		for _, b := range art.Blobs {
			_ = s.Registry.Blobs.Delete(r.Context(), b.Digest)
		}
	}
	_ = s.Registry.Meta.Delete(r.Context(), "nuget", id, ver)
	w.WriteHeader(http.StatusNoContent)
}

func (s *State) push(w http.ResponseWriter, r *http.Request) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	raw, _ := io.ReadAll(r.Body)
	ct := r.Header.Get("Content-Type")
	data := raw
	if strings.HasPrefix(ct, "multipart/form-data") {
		if _, part, ok := pkrkit.ExtractFirstFile(raw, ct); ok {
			data = part
		}
	}
	id, version := parseNuspec(data)
	if id == "" {
		id = "unknown"
	}
	id = lower(id)
	if version == "" {
		version = "0.1.0"
	}
	filename := id + "." + version + ".nupkg"
	storeVersionSource(s.Registry, id, version, filename, data, "push", r.Context())
	pkrkit.JSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func storeVersionSource(reg *pkrkit.Registry, id, ver, filename string, data []byte, source string, ctx context.Context) {
	if ver == "" {
		ver = "0.0.0"
	}
	art := pkrkit.Artifact{Format: "nuget", Repository: id, Version: ver, Source: source}
	if len(data) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(data)
		digest := "sha256:" + h.SHA256
		if _, err := reg.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(data)); err == nil {
			art.Blobs = append(art.Blobs, pkrkit.Descriptor{Digest: digest, Size: int64(len(data)), Name: filename})
		}
	}
	_ = reg.Meta.Put(ctx, art)
}

func parseNuspec(nupkg []byte) (string, string) {
	zr, err := zip.NewReader(bytes.NewReader(nupkg), int64(len(nupkg)))
	if err != nil {
		return "", ""
	}
	for _, f := range zr.File {
		if !strings.HasSuffix(strings.ToLower(f.Name), ".nuspec") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		buf, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		s := string(buf)
		id := extractXMLTag(s, "id")
		ver := extractXMLTag(s, "version")
		if id != "" && ver != "" {
			return id, ver
		}
	}
	return "", ""
}

func extractXMLTag(xml, tag string) string {
	start := strings.Index(xml, "<"+tag)
	if start < 0 {
		return ""
	}
	gtRel := strings.Index(xml[start:], ">")
	if gtRel < 0 {
		return ""
	}
	gt := start + gtRel + 1
	closeRel := strings.Index(xml[gt:], "</"+tag+">")
	if closeRel < 0 {
		return ""
	}
	return strings.TrimSpace(xml[gt : gt+closeRel])
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
