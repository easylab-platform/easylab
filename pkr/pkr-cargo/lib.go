// Package cargo implements the cargo registry protocol: sparse index (upstream
// page rewritten + local overlay), config.json, search, publish (binary
// framing), download with pull-through, yank/unyank, owners. Mirror of the
// pkglab reference.
package cargo

import (
	"bytes"
	"context"
	"encoding/binary"
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

func init() { pkrkit.Register("cargo", NewHandler) }

type meta struct {
	Yanked map[string]bool `json:"yanked"`
	Owners []string        `json:"owners"`
}

func (s *State) base() string {
	if s.SelfBase == "" {
		return "http://localhost:8080/pkgs/cargo"
	}
	return strings.TrimSuffix(s.SelfBase, "/")
}

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/cargo")
	path = strings.TrimPrefix(path, "/cargo")
	path = strings.Trim(path, "/")

	switch {
	case path == "config.json" || path == "index/config.json":
		s.config(w, r)
	case path == "index/config.json":
		s.config(w, r)
	case path == "me":
		pkrkit.JSON(w, http.StatusOK, map[string]any{"user": map[string]any{"id": 1, "login": "anonymous", "name": "anonymous"}})
	case strings.HasPrefix(path, "index/"):
		s.sparseIndex(w, r, strings.TrimPrefix(path, "index/"))
	case strings.HasPrefix(path, "api/v1/crates/new"):
		if r.Method == http.MethodPut {
			s.publish(w, r)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	case strings.HasPrefix(path, "api/v1/crates/") && strings.HasSuffix(path, "/download"):
		parts := strings.Split(strings.TrimSuffix(path, "/download"), "/")
		// api/v1/crates/{name}/{version}/download
		s.download(w, r, parts[3], parts[4])
	case strings.HasPrefix(path, "api/v1/crates/") && strings.HasSuffix(path, "/yank"):
		parts := strings.Split(strings.TrimSuffix(path, "/yank"), "/")
		s.yank(w, r, parts[3], parts[4], true)
	case strings.HasPrefix(path, "api/v1/crates/") && strings.HasSuffix(path, "/unyank"):
		parts := strings.Split(strings.TrimSuffix(path, "/unyank"), "/")
		s.yank(w, r, parts[3], parts[4], false)
	case strings.Contains(path, "/owners"):
		s.owners(w, r, path)
	case path == "api/v1/crates":
		if r.Method == http.MethodGet {
			s.search(w, r)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	case strings.HasPrefix(path, "api/v1/crates/"):
		// /api/v1/crates/{name}
		name := strings.TrimPrefix(path, "api/v1/crates/")
		s.apiFallback(w, r, name)
	default:
		// Cargo's sparse protocol appends the crate path to the registry base
		// directly (e.g. <base>/an/yh/anyhow), NOT under an `index/` prefix.
		// Serve any remaining GET as a sparse index entry.
		if r.Method == http.MethodGet {
			s.sparseIndex(w, r, strings.TrimPrefix(path, "index/"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *State) config(w http.ResponseWriter, r *http.Request) {
	pkrkit.JSON(w, http.StatusOK, map[string]any{"dl": s.base() + "/api/v1/crates", "api": s.base()})
}

func (s *State) search(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "cargo")
	var crates []any
	for _, name := range repos {
		if q != "" && !strings.Contains(strings.ToLower(name), q) {
			continue
		}
		versions, _ := s.Registry.Meta.ListVersions(r.Context(), "cargo", name)
		latest := pkrkit.HighestVersion(versions)
		if latest == "" && len(versions) > 0 {
			latest = versions[len(versions)-1]
		}
		crates = append(crates, map[string]any{"name": name, "max_version": latest})
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"crates": crates, "meta": map[string]any{"total": len(crates)}})
}

func (s *State) apiFallback(w http.ResponseWriter, r *http.Request, name string) {
	name = strings.Trim(name, "/")
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "cargo", name)
	if len(versions) == 0 {
		remote, err := s.Registry.Remote("cargo", "")
		if err == nil {
			if body, err := remote.GetBytes(r.Context(), "/api/v1/crates/"+pkrkit.URLencode(name)); err == nil {
				pkrkit.JSON(w, http.StatusOK, json.RawMessage(body))
				return
			}
		}
		pkrkit.Error(w, http.StatusNotFound, "crate not found")
		return
	}
	m := s.loadMeta(r.Context(), name)
	var vs []any
	for _, v := range versions {
		vs = append(vs, map[string]any{
			"crate_size": 0, "num": v, "dl_path": "/api/v1/crates/" + pkrkit.URLencode(name) + "/download",
			"yanked": m.Yanked[v],
		})
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"versions": vs})
}

func (s *State) sparseIndex(w http.ResponseWriter, r *http.Request, rel string) {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		pkrkit.Text(w, http.StatusOK, "", "text/plain")
		return
	}
	name := rel[strings.LastIndex(rel, "/")+1:]
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "cargo", name)
	pkrkit.SortSemver(versions)

	upstreamBody := s.fetchSparseIndex(rel)
	if len(versions) == 0 {
		if upstreamBody != "" {
			pkrkit.Text(w, http.StatusOK, upstreamBody, "text/plain")
			return
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var upstreamLines []string
	for _, line := range strings.Split(upstreamBody, "\n") {
		var mv map[string]any
		if json.Unmarshal([]byte(line), &mv) == nil {
			if v, ok := mv["vers"].(string); ok {
				if contains(versions, v) {
					continue
				}
			}
		}
		upstreamLines = append(upstreamLines, line)
	}
	var localLines []string
	m := s.loadMeta(r.Context(), name)
	for _, v := range versions {
		cksum := ""
		if art, err := s.Registry.Meta.Get(r.Context(), "cargo", name, v); err == nil && len(art.Blobs) > 0 {
			cksum = art.Blobs[0].Hex()
		}
		entry, _ := json.Marshal(map[string]any{
			"name": name, "vers": v, "deps": []any{}, "cksum": cksum, "features": map[string]any{},
			"yanked": m.Yanked[v], "links": nil,
		})
		localLines = append(localLines, string(entry))
	}
	out := strings.Join(upstreamLines, "\n")
	if len(localLines) > 0 {
		if out != "" {
			out += "\n"
		}
		out += strings.Join(localLines, "\n")
	}
	pkrkit.Text(w, http.StatusOK, out+"\n", "text/plain")
}

func (s *State) fetchSparseIndex(rel string) string {
	remote, err := s.registrySubRemote("cargo", "index")
	if err != nil {
		return ""
	}
	body, err := remote.GetCached(pkrkit.Ctx(), pkrkit.SharedIndexCache(), "/"+strings.Trim(rel, "/"))
	if err != nil {
		return ""
	}
	return s.rewriteSparseIndex(body)
}

func (s *State) rewriteSparseIndex(body string) string {
	var sb strings.Builder
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var mv map[string]any
		if json.Unmarshal([]byte(line), &mv) == nil {
			if name, ok := mv["name"].(string); ok && name != "" {
				mv["dl"] = s.base() + "/api/v1/crates"
				b, _ := json.Marshal(mv)
				sb.WriteString(string(b))
				sb.WriteByte('\n')
				continue
			}
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	return sb.String()
}

func (s *State) download(w http.ResponseWriter, r *http.Request, name, version string) {
	filename := name + "-" + version + ".crate"
	if art, err := s.Registry.Meta.Get(r.Context(), "cargo", name, version); err == nil {
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
	remote, err := s.registrySubRemote("cargo", "static")
	if err == nil {
		nameEnc := pkrkit.URLencode(name)
		cratePath := "/" + nameEnc + "/" + name + "-" + version + ".crate"
		apiPath := "/api/v1/crates/" + nameEnc + "/" + version + "/download"
		data, err := remote.GetBytes(r.Context(), cratePath)
		if err != nil {
			data, err = remote.GetBytes(r.Context(), apiPath)
		}
		if err == nil {
			storeVersionSource(s.Registry, name, version, data, "pull", r.Context())
			pkrkit.BlobResponse(w, data, filename)
			return
		}
	}
	pkrkit.Error(w, http.StatusNotFound, "not found")
}

func (s *State) publish(w http.ResponseWriter, r *http.Request) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	data, _ := io.ReadAll(r.Body)
	name, version, crate := parsePublishBody(data)
	if name == "" {
		name = r.URL.Query().Get("name")
	}
	if name == "" {
		name = jsonStr(data, "name")
		version = jsonStr(data, "version")
	}
	if version == "" {
		version = "0.1.0"
	}
	if name == "" {
		pkrkit.Error(w, http.StatusBadRequest, "missing name")
		return
	}
	if len(crate) == 0 {
		crate = data
	}
	storeVersionSource(s.Registry, name, version, crate, "push", r.Context())
	pkrkit.JSON(w, http.StatusCreated, map[string]any{"warnings": map[string]any{}})
}

func (s *State) yank(w http.ResponseWriter, r *http.Request, name, version string, yanked bool) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	if _, err := s.Registry.Meta.Get(r.Context(), "cargo", name, version); err != nil {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	m := s.loadMeta(r.Context(), name)
	if m.Yanked == nil {
		m.Yanked = map[string]bool{}
	}
	m.Yanked[version] = yanked
	s.saveMeta(r.Context(), name, m)
	pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *State) owners(w http.ResponseWriter, r *http.Request, path string) {
	full := strings.TrimPrefix(path, "api/v1/crates/")
	parts := strings.Split(full, "/")
	name := parts[0]
	// PUT/DELETE mutate the owner set; GET lists it.
	if (r.Method == http.MethodPut || r.Method == http.MethodDelete) && len(parts) >= 2 && parts[1] == "owners" {
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		data, _ := io.ReadAll(r.Body)
		m := s.loadMeta(r.Context(), name)
		var req struct {
			Users []string `json:"users"`
		}
		if json.Unmarshal(data, &req) == nil {
			if r.Method == http.MethodPut {
				for _, u := range req.Users {
					if !contains(m.Owners, u) {
						m.Owners = append(m.Owners, u)
					}
				}
				s.saveMeta(r.Context(), name, m)
			} else if r.Method == http.MethodDelete {
				var removed []string
				for _, u := range req.Users {
					if contains(m.Owners, u) {
						removed = append(removed, u)
					}
				}
				var kept []string
				for _, o := range m.Owners {
					if !contains(removed, o) {
						kept = append(kept, o)
					}
				}
				m.Owners = kept
				s.saveMeta(r.Context(), name, m)
			}
		}
		pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true, "msg": "owners updated"})
		return
	}
	m := s.loadMeta(r.Context(), name)
	var users []any
	for _, o := range m.Owners {
		users = append(users, map[string]any{"login": o, "name": o})
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"users": users})
}

func (s *State) loadMeta(ctx context.Context, name string) meta {
	m := meta{Yanked: map[string]bool{}}
	if art, err := s.Registry.Meta.Get(ctx, "cargo", name, ""); err == nil && len(art.Proprietary) > 0 {
		_ = json.Unmarshal(art.Proprietary, &m)
		if m.Yanked == nil {
			m.Yanked = map[string]bool{}
		}
	}
	return m
}

func (s *State) saveMeta(ctx context.Context, name string, m meta) {
	b, _ := json.Marshal(m)
	_ = s.Registry.Meta.Put(ctx, pkrkit.Artifact{Format: "cargo", Repository: name, Version: "", Proprietary: b})
}

func (s *State) registrySubRemote(format, sub string) (*pkrkit.Remote, error) {
	base := s.Registry.Upstreams.Sub(format, sub)
	if base == "" {
		base = s.Registry.Upstreams.Get(format)
	}
	if base == "" {
		return nil, errNoUpstream
	}
	return s.Registry.RemoteAt(base), nil
}

// parsePublishBody parses cargo's binary framing:
// [u32-le jsonLen][metadata JSON][u32-le crateLen][crate bytes].
func parsePublishBody(data []byte) (name, version string, crate []byte) {
	if len(data) < 8 {
		return "", "", nil
	}
	jsonLen := binary.LittleEndian.Uint32(data[0:4])
	if int(jsonLen) == 0 || 4+int(jsonLen) > len(data) {
		return "", "", nil
	}
	var mv map[string]any
	if json.Unmarshal(data[4:4+int(jsonLen)], &mv) != nil {
		return "", "", nil
	}
	name, _ = mv["name"].(string)
	version, _ = mv["vers"].(string)
	cratePos := 4 + int(jsonLen)
	if cratePos+4 > len(data) {
		return name, version, nil
	}
	crateLen := binary.LittleEndian.Uint32(data[cratePos : cratePos+4])
	end := cratePos + 4 + int(crateLen)
	if end > len(data) {
		end = len(data)
	}
	return name, version, data[cratePos+4 : end]
}

func storeVersionSource(reg *pkrkit.Registry, name, version string, data []byte, source string, ctx context.Context) {
	if version == "" {
		version = "0.1.0"
	}
	art := pkrkit.Artifact{Format: "cargo", Repository: name, Version: version, Source: source}
	if len(data) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(data)
		digest := "sha256:" + h.SHA256
		if _, err := reg.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(data)); err == nil {
			art.Blobs = append(art.Blobs, pkrkit.Descriptor{Digest: digest, Size: int64(len(data)), Name: name + "-" + version + ".crate"})
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

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var errNoUpstream = errorString("no upstream")

type errorString string

func (e errorString) Error() string { return string(e) }
