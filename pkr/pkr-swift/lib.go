// Package swift implements the Swift Package Registry (SE-0321): releases,
// source archives, Package.swift, publish, identifiers, login. Mirror of the
// pkglab reference (SCM-to-registry stubs are omitted; pull-through supported).
package swift

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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

func init() { pkrkit.Register("swift", NewHandler) }

func (s *State) base() string {
	if s.SelfBase == "" {
		return "http://localhost:8080/pkgs/swift"
	}
	return strings.TrimSuffix(s.SelfBase, "/")
}

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/swift")
	path = strings.TrimPrefix(path, "/swift")
	path = strings.Trim(path, "/")
	method := r.Method

	switch {
	case path == "" || path == "/":
		if method == http.MethodPost || method == http.MethodGet {
			s.login(w, r)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	case path == "login":
		s.login(w, r)
	case path == "identifiers":
		s.identifiers(w, r)
	case method == http.MethodOptions:
		w.Header().Set("Allow", "GET, HEAD, PUT, OPTIONS")
		w.Header().Set("Link", `<https://github.com/swiftlang/swift-package-manager/blob/main/Documentation/PackageRegistry/Registry.md>; rel="service-doc"`)
		w.Header().Set("Content-Version", "1")
		w.WriteHeader(http.StatusOK)
	case method == http.MethodPut:
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		s.putPath(w, r, path)
	case method == http.MethodGet || method == http.MethodHead:
		parts := strings.Split(path, "/")
		if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
			pkrkit.Error(w, http.StatusNotFound, "invalid path")
			return
		}
		scope, name := parts[0], parts[1]
		id := scope + "." + name
		if len(parts) == 2 {
			s.releases(w, r, scope, name, id)
		} else if len(parts) == 3 {
			ver := parts[2]
			if strings.HasSuffix(ver, ".zip") {
				s.sourceZip(w, r, id, strings.TrimSuffix(ver, ".zip"), name)
			} else {
				s.versionMeta(w, r, id, ver)
			}
		} else if len(parts) == 4 && parts[3] == "Package.swift" {
			s.packageSwift(w, r, id, parts[2], name)
		} else {
			pkrkit.Error(w, http.StatusNotFound, "release not found")
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *State) login(w http.ResponseWriter, r *http.Request) {
	if s.Auth != nil {
		if u := s.Auth.Authenticate(r.Context(), r); u != "" {
			tok := s.Auth.IssueToken(r.Context(), u, nil, 3600)
			pkrkit.JSON(w, http.StatusOK, map[string]any{"token": tok})
			return
		}
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"token": "valid"})
}

func (s *State) identifiers(w http.ResponseWriter, r *http.Request) {
	if urlParam := r.URL.Query().Get("url"); urlParam != "" {
		id := strings.ReplaceAll(strings.ReplaceAll(urlParam, "%3A", ":"), "%2F", "/")
		scope, name := idIdentity(id)
		result := scope + "." + name
		// Persist the git URL so SCM-to-registry can enumerate tags + serve
		// source archives (mirrors the reference `swift-git:{id}` meta slot).
		_ = s.Registry.Meta.SetMeta(r.Context(), "swift", "swift-git:"+result, []byte(id))
		pkrkit.JSON(w, http.StatusOK, map[string]any{"identifiers": []string{result}})
		return
	}
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "swift")
	pkrkit.JSON(w, http.StatusOK, map[string]any{"identifiers": repos})
}

// gitURLFor returns the persisted git URL for an identifier ("" when absent).
func (s *State) gitURLFor(ctx context.Context, id string) string {
	if b, err := s.Registry.Meta.GetMeta(ctx, "swift", "swift-git:"+id); err == nil {
		return string(b)
	}
	return ""
}

// gitTags lists the semver-looking tags of a remote git repo.
func (s *State) gitTags(ctx context.Context, url string) []string {
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(tctx, "git", "ls-remote", "--tags", "--refs", url)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var tags []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		ref := fields[1]
		tag := ref[strings.LastIndex(ref, "/")+1:]
		// Only semver-ish tags; strip a leading "v".
		v := strings.TrimPrefix(tag, "v")
		if _, _, _, _, ok := tryParseSemver(v); ok {
			tags = append(tags, v)
		}
	}
	return tags
}

func tryParseSemver(raw string) (major, minor, patch int, prerelease string, ok bool) {
	s := strings.TrimSpace(raw)
	if len(s) > 0 && (s[0] == 'v' || s[0] == 'V') {
		s = s[1:]
	}
	if s == "" {
		return 0, 0, 0, "", false
	}
	core := s
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		core = s[:i]
		prerelease = s[i:]
	}
	parts := strings.Split(core, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return 0, 0, 0, "", false
	}
	var nums []int
	for _, p := range parts {
		var n int
		if p == "" {
			return 0, 0, 0, "", false
		}
		for _, c := range p {
			if c < '0' || c > '9' {
				return 0, 0, 0, "", false
			}
			n = n*10 + int(c-'0')
		}
		nums = append(nums, n)
	}
	for len(nums) < 3 {
		nums = append(nums, 0)
	}
	return nums[0], nums[1], nums[2], prerelease, true
}

// gitArchive builds a source archive (zip) of a repo at a tag using
// `git archive`. Falls back to an empty zip on error.
func (s *State) gitArchive(ctx context.Context, url, tag string) []byte {
	dir, err := os.MkdirTemp("", "pkr-swift-git-*")
	if err != nil {
		return nil
	}
	defer os.RemoveAll(dir)
	tctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if err := exec.CommandContext(tctx, "git", "clone", "--depth", "1", "--branch", tag, url, filepath.Join(dir, "repo")).Run(); err != nil {
		return nil
	}
	cmd := exec.CommandContext(tctx, "git", "-C", filepath.Join(dir, "repo"), "archive", "--format=zip", "-o", filepath.Join(dir, "out.zip"), "HEAD")
	if err := cmd.Run(); err != nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "out.zip"))
	if err != nil {
		return nil
	}
	return data
}

func idIdentity(url string) (string, string) {
	u := strings.TrimPrefix(url, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimSuffix(strings.TrimSuffix(u, ".git"), "/")
	parts := strings.Split(u, "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2], parts[len(parts)-1]
	}
	return "local", "package"
}

func (s *State) releases(w http.ResponseWriter, r *http.Request, scope, name, id string) {
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "swift", id)
	rels := map[string]any{}
	for _, v := range versions {
		rels[v] = map[string]any{"url": s.base() + "/" + pkrkit.URLencode(scope) + "/" + pkrkit.URLencode(name) + "/" + v + ".zip"}
	}
	if len(rels) == 0 {
		// SCM-to-registry: if a git URL was persisted for this id, enumerate
		// its tags as releases (each served as a source archive zip below).
		if gu := s.gitURLFor(r.Context(), id); gu != "" {
			if tags := s.gitTags(r.Context(), gu); len(tags) > 0 {
				for _, t := range tags {
					rels[t] = map[string]any{"url": s.base() + "/" + pkrkit.URLencode(scope) + "/" + pkrkit.URLencode(name) + "/" + t + ".zip"}
				}
				w.Header().Set("Content-Version", "1")
				pkrkit.JSON(w, http.StatusOK, map[string]any{"releases": rels})
				return
			}
		}
		remote, err := s.Registry.Remote("swift", "")
		if err == nil {
			if body, err := remote.GetCached(r.Context(), pkrkit.SharedIndexCache(), "/"+pkrkit.URLencode(scope)+"="+pkrkit.URLencode(name)); err == nil {
				w.Header().Set("Content-Version", "1")
				pkrkit.JSON(w, http.StatusOK, json.RawMessage(body))
				return
			}
		}
	}
	w.Header().Set("Content-Version", "1")
	pkrkit.JSON(w, http.StatusOK, map[string]any{"releases": rels})
}

func (s *State) versionMeta(w http.ResponseWriter, r *http.Request, id, version string) {
	checksum := ""
	if art, err := s.Registry.Meta.Get(r.Context(), "swift", id, version); err == nil && len(art.Blobs) > 0 {
		checksum = art.Blobs[0].Hex()
	}
	w.Header().Set("Content-Version", "1")
	pkrkit.JSON(w, http.StatusOK, map[string]any{
		"id": id, "version": version,
		"resources": []any{map[string]any{"name": "source-archive", "type": "application/zip", "checksum": checksum}},
		"metadata":  map[string]any{"description": ""}, "publishedAt": "2024-01-01T00:00:00Z",
	})
}

func (s *State) sourceZip(w http.ResponseWriter, r *http.Request, id, version, name string) {
	filename := name + "-" + version + ".zip"
	if art, err := s.Registry.Meta.Get(r.Context(), "swift", id, version); err == nil {
		for _, b := range art.Blobs {
			rd, err := s.Registry.Blobs.Open(r.Context(), b.Digest)
			if err != nil || rd == nil {
				continue
			}
			data, _ := io.ReadAll(rd)
			rd.Close()
			w.Header().Set("Content-Type", "application/zip")
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			w.WriteHeader(http.StatusOK)
			w.Write(data)
			return
		}
	}
	fetched, err := s.Registry.Fetch(r.Context(), "swift", "", "/"+pkrkit.URLencode(id)+"/"+version+".zip")
	if err != nil {
		// SCM-to-registry: build a source archive from the persisted git repo.
		if gu := s.gitURLFor(r.Context(), id); gu != "" {
			if data := s.gitArchive(r.Context(), gu, version); len(data) > 0 {
				storeVersionSource(s.Registry, id, version, filename, data, "pull", r.Context())
				w.Header().Set("Content-Type", "application/zip")
				w.Header().Set("Content-Length", fmt.Sprint(len(data)))
				w.WriteHeader(http.StatusOK)
				w.Write(data)
				return
			}
		}
		pkrkit.JSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	storeVersionSource(s.Registry, id, version, filename, fetched.Data, "pull", r.Context())
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Length", fmt.Sprint(len(fetched.Data)))
	w.WriteHeader(http.StatusOK)
	w.Write(fetched.Data)
}

func (s *State) packageSwift(w http.ResponseWriter, r *http.Request, id, version, name string) {
	if art, err := s.Registry.Meta.Get(r.Context(), "swift", id, version); err == nil {
		for _, b := range art.Blobs {
			rd, err := s.Registry.Blobs.Open(r.Context(), b.Digest)
			if err != nil || rd == nil {
				continue
			}
			data, _ := io.ReadAll(rd)
			rd.Close()
			if manifest, ok := extractPackageSwift(data); ok {
				w.Header().Set("Content-Version", "1")
				pkrkit.Text(w, http.StatusOK, manifest, "text/x-swift")
				return
			}
		}
	}
	manifest := "// swift-tools-version:5.9\nimport PackageDescription\n\nlet package = Package(\n    name: \"" + name + "\",\n    products: [.library(name: \"" + name + "\", targets: [\"" + name + "\"])],\n    targets: [.target(name: \"" + name + "\")]\n)\n"
	w.Header().Set("Content-Version", "1")
	pkrkit.Text(w, http.StatusOK, manifest, "text/x-swift")
}

func (s *State) putPath(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(path, "/")
	if len(parts) != 3 {
		pkrkit.JSON(w, http.StatusBadRequest, map[string]any{"error": "invalid upload path"})
		return
	}
	scope, name, ver := parts[0], parts[1], parts[2]
	full := scope + "." + name
	raw, _ := io.ReadAll(r.Body)
	ct := r.Header.Get("Content-Type")
	zipData := raw
	if strings.HasPrefix(ct, "multipart/form-data") {
		if d, ok := pkrkit.ExtractField(raw, ct, "source-archive"); ok {
			zipData = d
		}
	}
	if len(zipData) == 0 {
		pkrkit.JSON(w, http.StatusBadRequest, map[string]any{"error": "missing source-archive"})
		return
	}
	h, _ := pkrkit.ComputeHashesBytes(zipData)
	filename := name + "-" + ver + ".zip"
	storeVersionSource(s.Registry, full, ver, filename, zipData, "push", r.Context())
	w.Header().Set("Content-Version", "1")
	w.Header().Set("Location", s.base()+"/"+pkrkit.URLencode(scope)+"/"+pkrkit.URLencode(name)+"/"+ver+".zip")
	pkrkit.JSON(w, http.StatusCreated, map[string]any{
		"id": full, "version": ver,
		"resources": []any{map[string]any{"name": "source-archive", "type": "application/zip", "checksum": h.SHA256}},
	})
}

func storeVersionSource(reg *pkrkit.Registry, full, version, filename string, data []byte, source string, ctx context.Context) {
	art := pkrkit.Artifact{Format: "swift", Repository: full, Version: version, Source: source}
	if len(data) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(data)
		digest := "sha256:" + h.SHA256
		if _, err := reg.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(data)); err == nil {
			art.Blobs = append(art.Blobs, pkrkit.Descriptor{Digest: digest, Size: int64(len(data)), Name: filename})
		}
	}
	_ = reg.Meta.Put(ctx, art)
}

func extractPackageSwift(zipData []byte) (string, bool) {
	zr, err := zip.NewReader(bytes.NewReader(zipData), int64(len(zipData)))
	if err != nil {
		return "", false
	}
	for _, f := range zr.File {
		if f.Name == "Package.swift" || strings.HasSuffix(f.Name, "/Package.swift") {
			rc, _ := f.Open()
			buf, _ := io.ReadAll(io.LimitReader(rc, 1<<20))
			rc.Close()
			return string(buf), true
		}
	}
	return "", false
}
