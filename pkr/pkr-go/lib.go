// Package golang implements the Go module proxy (GOPROXY) protocol:
// @v/list, @latest, .info/.mod/.zip with pull-through and a local PUT
// /upload, mirroring the pkglab reference.
package golang

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/pkr/pkrkit"
)

type State struct {
	Registry *pkrkit.Registry
	Auth     pkrkit.Auth
}

func NewHandler(reg *pkrkit.Registry, cfg map[string]any) (http.Handler, error) {
	s := &State{Registry: reg}
	if a, ok := cfg["auth"].(pkrkit.Auth); ok {
		s.Auth = a
	}
	return s, nil
}

func init() { pkrkit.Register("go", NewHandler) }

// EncodeModulePath escapes uppercase letters as !lower per the Go proxy spec.
func EncodeModulePath(m string) string {
	var b strings.Builder
	for _, c := range m {
		if c >= 'A' && c <= 'Z' {
			b.WriteByte('!')
			b.WriteRune(c - 'A' + 'a')
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/go")
	path = strings.TrimPrefix(path, "/go")
	path = strings.Trim(path, "/")

	if path == "upload" && r.Method == http.MethodPut {
		s.upload(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	switch {
	case strings.HasSuffix(path, "/@latest"):
		s.latest(w, r, strings.TrimSuffix(path, "/@latest"))
	case strings.HasSuffix(path, "/@v/list"):
		s.versions(w, r, strings.TrimSuffix(path, "/@v/list"))
	default:
		for _, ext := range []string{".info", ".mod", ".zip"} {
			if strings.HasSuffix(path, ext) {
				rest := strings.TrimSuffix(path, ext)
				mod, ver := splitModuleVersion(rest)
				if ver == "" {
					break
				}
				switch ext {
				case ".info":
					s.info(w, r, mod, ver)
				case ".mod":
					s.goMod(w, r, mod, ver)
				case ".zip":
					s.moduleZip(w, r, mod, ver)
				}
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

func splitModuleVersion(rest string) (string, string) {
	if i := strings.Index(rest, "/@v/"); i >= 0 {
		return rest[:i], rest[i+len("/@v/"):]
	}
	return rest, ""
}

func (s *State) latest(w http.ResponseWriter, r *http.Request, module string) {
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "go", module)
	if v := pkrkit.HighestVersion(versions); v != "" {
		s.jsonOk(w, map[string]any{"Version": v, "Time": "2024-01-01T00:00:00Z"})
		return
	}
	if len(versions) > 0 {
		s.jsonOk(w, map[string]any{"Version": versions[len(versions)-1], "Time": "2024-01-01T00:00:00Z"})
		return
	}
	s.proxyOne(w, r, module, "@latest", "application/json")
}

func (s *State) versions(w http.ResponseWriter, r *http.Request, module string) {
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "go", module)
	if len(versions) > 0 {
		pkrkit.SortSemver(versions)
		pkrkit.Text(w, http.StatusOK, strings.Join(versions, "\n"), "text/plain")
		return
	}
	s.proxyOne(w, r, module, "@v/list", "text/plain")
}

func (s *State) info(w http.ResponseWriter, r *http.Request, module, version string) {
	if _, err := s.Registry.Meta.Get(r.Context(), "go", module, version); err == nil {
		s.jsonOk(w, map[string]any{"Version": version, "Time": "2024-01-01T00:00:00Z"})
		return
	}
	s.proxyOne(w, r, module, "@v/"+version+".info", "application/json")
}

func (s *State) goMod(w http.ResponseWriter, r *http.Request, module, version string) {
	if art, err := s.Registry.Meta.Get(r.Context(), "go", module, version); err == nil {
		for _, b := range art.Blobs {
			rd, err := s.Registry.Blobs.Open(r.Context(), b.Digest)
			if err != nil || rd == nil {
				continue
			}
			data, _ := io.ReadAll(rd)
			rd.Close()
			if gm := extractGoMod(data); gm != "" {
				pkrkit.Text(w, http.StatusOK, gm, "text/plain; charset=utf-8")
				return
			}
		}
	}
	s.proxyOne(w, r, module, "@v/"+version+".mod", "text/plain; charset=utf-8")
}

func (s *State) moduleZip(w http.ResponseWriter, r *http.Request, module, version string) {
	filename := module + "-" + version + ".zip"
	if art, err := s.Registry.Meta.Get(r.Context(), "go", module, version); err == nil {
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
	ep := EncodeModulePath(module)
	body, err := s.registryFetch(r.Context(), "/"+ep+"/@v/"+version+".zip")
	if err != nil {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	storeVersionSource(s.Registry, module, version, body, "pull", r.Context())
	pkrkit.BlobResponse(w, body, filename)
}

func (s *State) proxyOne(w http.ResponseWriter, r *http.Request, module, suffix, ct string) {
	ep := EncodeModulePath(module)
	body, err := s.registryFetch(r.Context(), "/"+ep+"/"+suffix)
	if err != nil {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	pkrkit.Text(w, http.StatusOK, string(body), ct)
}

func (s *State) registryFetch(ctx context.Context, path string) ([]byte, error) {
	remote, err := s.Registry.Remote("go", "")
	if err != nil {
		return nil, err
	}
	return remote.GetBytes(ctx, path)
}

func (s *State) jsonOk(w http.ResponseWriter, v any) {
	pkrkit.JSON(w, http.StatusOK, v)
}

func (s *State) upload(w http.ResponseWriter, r *http.Request) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	name, version := r.URL.Query().Get("name"), r.URL.Query().Get("version")
	if name == "" {
		name = r.URL.Query().Get("module")
	}
	data, _ := io.ReadAll(r.Body)
	if name == "" {
		name = jsonStr(data, "name")
		if name == "" {
			name = jsonStr(data, "module")
		}
		if version == "" {
			version = jsonStr(data, "version")
		}
	}
	if name == "" {
		name = "go/local"
	}
	if version == "" {
		version = "v0.0.0"
	}
	storeVersionSource(s.Registry, name, version, data, "push", r.Context())
	pkrkit.JSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func storeVersionSource(reg *pkrkit.Registry, module, version string, data []byte, source string, ctx context.Context) {
	if version == "" {
		version = "v0.0.0"
	}
	art := pkrkit.Artifact{Format: "go", Repository: module, Version: version, Source: source}
	if len(data) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(data)
		digest := "sha256:" + h.SHA256
		if _, err := reg.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(data)); err == nil {
			art.Blobs = append(art.Blobs, pkrkit.Descriptor{Digest: digest, Size: int64(len(data)), Name: module + "-" + version + ".zip"})
		}
	}
	_ = reg.Meta.Put(ctx, art)
}

func jsonStr(data []byte, key string) string {
	var m map[string]any
	if json.Unmarshal(data, &m) == nil {
		if v, ok := m[key]; ok {
			if s, ok := v.(string); ok {
				return s
			}
		}
	}
	return ""
}

func extractGoMod(zipData []byte) string {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return ""
	}
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/go.mod") || f.Name == "go.mod" {
			rc, err := f.Open()
			if err != nil {
				return ""
			}
			buf, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			return string(buf)
		}
	}
	return ""
}
