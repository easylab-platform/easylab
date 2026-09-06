// Package pub implements the Dart pub.dev protocol: package metadata, archive
// download with pull-through, two-phase upload, pubspec parsing. Mirror of the
// pkglab reference.
package pub

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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

func init() { pkrkit.Register("pub", NewHandler) }

func (s *State) base() string {
	if s.SelfBase == "" {
		return "http://localhost:8080/pkgs/pub"
	}
	return strings.TrimSuffix(s.SelfBase, "/")
}

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/pub")
	path = strings.TrimPrefix(path, "/pub")
	path = strings.Trim(path, "/")

	switch {
	case path == "" || path == "/":
		pkrkit.JSON(w, http.StatusOK, map[string]any{"name": "pkr-pub"})
	case path == "api/packages":
		s.packagesList(w, r)
	case path == "api/packages/versions/new":
		s.versionsNew(w, r)
	case path == "api/packages/versions/newUpload":
		s.newUpload(w, r)
	case path == "api/packages/versions/newUploadFinish":
		s.finish(w, r)
	case strings.HasPrefix(path, "api/packages/") && strings.HasSuffix(path, "/advisories"):
		pkrkit.JSON(w, http.StatusOK, map[string]any{"advisories": []any{}, "advisoriesUpdated": "1970-01-01T00:00:00Z"})
	case strings.HasPrefix(path, "api/packages/") && strings.Contains(path, "/versions/"):
		s.versionArchive(w, r, path)
	case strings.HasPrefix(path, "api/packages/"):
		name := strings.TrimPrefix(path, "api/packages/")
		if r.Method == http.MethodDelete {
			s.retract(w, r, name)
			return
		}
		s.pkgMetadata(w, r, name)
	case strings.HasPrefix(path, "packages/"):
		s.archive(w, r, strings.TrimPrefix(path, "packages/"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *State) packagesList(w http.ResponseWriter, r *http.Request) {
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "pub")
	pkrkit.JSON(w, http.StatusOK, map[string]any{"packages": repos, "next_url": "", "total": len(repos)})
}

func (s *State) versionsNew(w http.ResponseWriter, r *http.Request) {
	base := s.base()
	pkrkit.JSON(w, http.StatusOK, map[string]any{"url": base + "/api/packages/versions/newUpload", "fields": map[string]any{}})
}

func (s *State) newUpload(w http.ResponseWriter, r *http.Request) {
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
	name, version := r.URL.Query().Get("name"), r.URL.Query().Get("version")
	if name == "" || version == "" {
		pn, pv := pubspecNameVersion(data)
		if name == "" {
			name = pn
		}
		if version == "" {
			version = pv
		}
	}
	if name == "" {
		name = "unknown"
	}
	if version == "" {
		version = "0.1.0"
	}
	filename := name + "-" + version + ".tar.gz"
	storeVersionSource(s.Registry, name, version, filename, data, "push", r.Context())
	w.Header().Set("Location", s.base()+"/api/packages/versions/newUploadFinish")
	pkrkit.JSON(w, http.StatusCreated, map[string]any{"success": map[string]any{"message": "Package uploaded"}})
}

func (s *State) finish(w http.ResponseWriter, r *http.Request) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"success": map[string]any{"message": "Successfully uploaded package."}})
}

func (s *State) pkgMetadata(w http.ResponseWriter, r *http.Request, name string) {
	name = strings.Trim(name, "/")
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "pub", name)
	if len(versions) == 0 {
		remote, err := s.Registry.Remote("pub", "")
		if err == nil {
			if body, err := remote.GetCached(r.Context(), pkrkit.SharedIndexCache(), "/api/packages/"+pkrkit.URLencode(name)); err == nil {
				pkrkit.JSON(w, http.StatusOK, json.RawMessage(body))
				return
			}
		}
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	pkrkit.SortSemver(versions)
	base := s.base()
	latest := pkrkit.HighestVersion(versions)
	if latest == "" && len(versions) > 0 {
		latest = versions[len(versions)-1]
	}
	var vs []any
	for _, v := range versions {
		sha := ""
		if art, err := s.Registry.Meta.Get(r.Context(), "pub", name, v); err == nil && len(art.Blobs) > 0 {
			sha = art.Blobs[0].Hex()
		}
		vs = append(vs, map[string]any{
			"version": v, "pubspec": map[string]any{"name": name, "version": v, "environment": map[string]any{"sdk": ">=3.0.0 <4.0.0"}},
			"archive_url": base + "/packages/" + pkrkit.URLencode(name) + "/" + v + ".tar.gz", "archive_sha256": sha,
		})
	}
	latestSHA := ""
	if art, err := s.Registry.Meta.Get(r.Context(), "pub", name, latest); err == nil && len(art.Blobs) > 0 {
		latestSHA = art.Blobs[0].Hex()
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{
		"name":     name,
		"latest":   map[string]any{"version": latest, "archive_url": base + "/packages/" + pkrkit.URLencode(name) + "/" + latest + ".tar.gz", "archive_sha256": latestSHA, "pubspec": map[string]any{"name": name, "version": latest, "environment": map[string]any{"sdk": ">=3.0.0 <4.0.0"}}},
		"versions": vs,
	})
}

func (s *State) versionArchive(w http.ResponseWriter, r *http.Request, path string) {
	rest := strings.TrimPrefix(path, "api/packages/")
	parts := strings.Split(rest, "/")
	if len(parts) < 3 {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	name, version := parts[0], parts[2]
	if strings.HasSuffix(version, ".tar.gz") {
		v := strings.TrimSuffix(version, ".tar.gz")
		filename := name + "-" + v + ".tar.gz"
		if art, err := s.Registry.Meta.Get(r.Context(), "pub", name, v); err == nil {
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
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"name": name, "version": version, "archive_url": s.base() + "/packages/" + pkrkit.URLencode(name) + "/" + version + ".tar.gz", "pubspec": map[string]any{"name": name, "version": version}})
}

func (s *State) archive(w http.ResponseWriter, r *http.Request, rest string) {
	rel := strings.Trim(rest, "/")
	filename := rel[strings.LastIndex(rel, "/")+1:]
	stem := strings.TrimSuffix(filename, ".tar.gz")
	var name, version string
	if strings.Contains(rel, "/") {
		name = strings.Split(rel, "/")[0]
		version = stem
	} else {
		name, version = nameVersionFromStem(stem)
	}
	fullName := name + "-" + stem + ".tar.gz"
	if art, err := s.Registry.Meta.Get(r.Context(), "pub", name, version); err == nil {
		for _, b := range art.Blobs {
			if b.Name == filename || b.Name == fullName || strings.HasSuffix(b.Name, filename) {
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
	fetched, err := s.Registry.Fetch(r.Context(), "pub", "", "/packages/"+fullName)
	if err != nil {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	storeVersionSource(s.Registry, name, version, filename, fetched.Data, "pull", r.Context())
	pkrkit.BlobResponse(w, fetched.Data, filename)
}

func (s *State) retract(w http.ResponseWriter, r *http.Request, name string) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	name = strings.Trim(name, "/")
	vs, _ := s.Registry.Meta.ListVersions(r.Context(), "pub", name)
	for _, v := range vs {
		removeVersion(s.Registry, name, v, r.Context())
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func nameVersionFromStem(stem string) (string, string) {
	if i := strings.LastIndex(stem, "-"); i > 0 {
		return stem[:i], stem[i+1:]
	}
	return stem, "0.0.0"
}

func removeVersion(reg *pkrkit.Registry, name, version string, ctx context.Context) {
	if art, err := reg.Meta.Get(ctx, "pub", name, version); err == nil {
		for _, b := range art.Blobs {
			_ = reg.Blobs.Delete(ctx, b.Digest)
		}
	}
	_ = reg.Meta.Delete(ctx, "pub", name, version)
}

func storeVersionSource(reg *pkrkit.Registry, name, version, filename string, data []byte, source string, ctx context.Context) {
	art := pkrkit.Artifact{Format: "pub", Repository: name, Version: version, Source: source}
	if len(data) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(data)
		digest := "sha256:" + h.SHA256
		if _, err := reg.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(data)); err == nil {
			art.Blobs = append(art.Blobs, pkrkit.Descriptor{Digest: digest, Size: int64(len(data)), Name: filename})
		}
	}
	_ = reg.Meta.Put(ctx, art)
}

func pubspecNameVersion(data []byte) (string, string) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", ""
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
		if !strings.HasSuffix(hdr.Name, "pubspec.yaml") {
			continue
		}
		buf, _ := io.ReadAll(io.LimitReader(tr, 1<<20))
		var name, version string
		for _, line := range strings.Split(string(buf), "\n") {
			t := strings.TrimSpace(line)
			if rest, ok := strings.CutPrefix(t, "name:"); ok {
				name = strings.TrimSpace(rest)
			}
			if rest, ok := strings.CutPrefix(t, "version:"); ok {
				version = strings.Trim(strings.TrimSpace(rest), `"'`)
			}
		}
		return name, version
	}
	return "", ""
}
