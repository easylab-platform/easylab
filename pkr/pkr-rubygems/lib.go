// Package rubygems implements the RubyGems protocol: compact index,
// specs.4.8.gz (Ruby Marshal), gem download with pull-through, upload, yank,
// search, owners, api_key stubs, dependencies. Mirror of the pkglab reference
// (includes the byte-exact Ruby Marshal 4.8 encoders).
package rubygems

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"io"
	"net/url"
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

func init() { pkrkit.Register("rubygems", NewHandler) }

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/rubygems")
	path = strings.TrimPrefix(path, "/rubygems")
	path = strings.Trim(path, "/")

	switch {
	case path == "specs.4.8.gz" || path == "latest_specs.4.8.gz" || path == "prerelease_specs.4.8.gz":
		s.specs(w, r)
	case path == "names":
		s.names(w, r)
	case path == "versions":
		s.compactVersions(w, r)
	case strings.HasPrefix(path, "info/"):
		s.compactInfo(w, r, strings.TrimPrefix(path, "info/"))
	case strings.HasPrefix(path, "api/v1/versions/"):
		rest := strings.TrimPrefix(path, "api/v1/versions/")
		if strings.HasSuffix(rest, "/latest") || strings.HasSuffix(rest, "/latest.json") {
			s.latest(w, r, strings.TrimSuffix(strings.TrimSuffix(rest, "/latest"), ".json"))
			return
		}
		s.versionsAPI(w, r, strings.TrimSuffix(rest, ".json"))
	case strings.HasPrefix(path, "api/v1/gems") && strings.HasSuffix(path, "/owners"):
		s.owners(w, r)
	case path == "api/v1/gems/yank":
		// RubyGems `gem yank` sends DELETE .../yank with gem_name + version
		// in the url-encoded request BODY (not the query string). Read both.
		// NOTE: must precede the generic ".../gems/{name}/yank" prefix case,
		// which would otherwise match this exact path.
		s.yankFromRequest(w, r)
	case strings.HasPrefix(path, "api/v1/gems/") && strings.HasSuffix(path, "/yank"):
		if r.Method == http.MethodDelete {
			name := strings.TrimSuffix(strings.TrimPrefix(path, "api/v1/gems/"), "/yank")
			s.yank(w, r, name, r.URL.Query().Get("version"))
		}
	case strings.HasPrefix(path, "api/v2/rubygems/"):
		s.versionV2(w, r, path)
	case path == "api/v1/gems":
		if r.Method == http.MethodPost {
			s.upload(w, r)
			return
		}
		s.gemsList(w, r)
	case strings.HasPrefix(path, "api/v1/gems/"):
		s.gemsInfo(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "api/v1/gems/"), ".json"))
	case path == "api/v1/dependencies":
		s.dependencies(w, r)
	case path == "api/v1/search" || path == "api/v1/search.json" || path == "api/v1/search.yaml":
		s.searchGem(w, r)
	case path == "api/v1/api_key":
		pkrkit.JSON(w, http.StatusOK, map[string]any{"rubygems_api_key": "test-token", "name": "test"})
	case strings.HasPrefix(path, "gems/"):
		s.download(w, r, strings.TrimPrefix(path, "gems/"))
	case strings.HasPrefix(path, "quick/"):
		s.quickMarshal(w, r, strings.TrimPrefix(path, "quick/"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (s *State) names(w http.ResponseWriter, r *http.Request) {
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "rubygems")
	pkrkit.Text(w, http.StatusOK, strings.Join(repos, "\n"), "text/plain")
}

func (s *State) versionsAPI(w http.ResponseWriter, r *http.Request, name string) {
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "rubygems", name)
	if len(versions) == 0 {
		remote, _ := s.Registry.Remote("rubygems", "")
		if remote != nil {
			if body, err := remote.GetBytes(r.Context(), "/api/v1/versions/"+pkrkit.URLencode(name)+".json"); err == nil {
				pkrkit.JSON(w, http.StatusOK, json.RawMessage(body))
				return
			}
		}
	}
	var out []any
	for _, v := range versions {
		out = append(out, map[string]any{"number": v, "platform": "ruby", "created_at": "2024-01-01T00:00:00Z"})
	}
	pkrkit.JSON(w, http.StatusOK, out)
}

func (s *State) latest(w http.ResponseWriter, r *http.Request, name string) {
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "rubygems", name)
	pkrkit.JSON(w, http.StatusOK, map[string]any{"version": pkrkit.HighestVersion(versions)})
}

func (s *State) gemsInfo(w http.ResponseWriter, r *http.Request, name string) {
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "rubygems", name)
	if len(versions) == 0 {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	var lines []string
	for _, v := range versions {
		lines = append(lines, v+" |sha256:|")
	}
	pkrkit.Text(w, http.StatusOK, "---\n"+strings.Join(lines, "\n"), "text/plain")
}

func (s *State) gemsList(w http.ResponseWriter, r *http.Request) {
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "rubygems")
	var out []any
	for _, name := range repos {
		versions, _ := s.Registry.Meta.ListVersions(r.Context(), "rubygems", name)
		out = append(out, map[string]any{"name": name, "version": pkrkit.HighestVersion(versions)})
	}
	pkrkit.JSON(w, http.StatusOK, out)
}

func (s *State) owners(w http.ResponseWriter, r *http.Request) {
	pkrkit.JSON(w, http.StatusOK, []any{map[string]any{"id": 1, "handle": "anonymous", "role": "owner"}})
}

func (s *State) searchGem(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("query"))
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "rubygems")
	var out []any
	for _, name := range repos {
		if q != "" && !strings.Contains(strings.ToLower(name), q) {
			continue
		}
		versions, _ := s.Registry.Meta.ListVersions(r.Context(), "rubygems", name)
		out = append(out, map[string]any{"name": name, "version": pkrkit.HighestVersion(versions)})
	}
	pkrkit.JSON(w, http.StatusOK, out)
}

func (s *State) versionV2(w http.ResponseWriter, r *http.Request, path string) {
	// /api/v2/rubygems/{name}/versions/{version}
	rest := strings.TrimPrefix(path, "api/v2/rubygems/")
	parts := strings.Split(rest, "/")
	if len(parts) >= 3 {
		name := strings.TrimSuffix(parts[0], ".json")
		ver := strings.TrimSuffix(parts[2], ".json")
		pkrkit.JSON(w, http.StatusOK, map[string]any{"name": name, "version": ver, "platform": "ruby", "number": ver, "created_at": "2024-01-01T00:00:00Z"})
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (s *State) compactVersions(w http.ResponseWriter, r *http.Request) {
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "rubygems")
	var sb strings.Builder
	sb.WriteString("created_at: 2026-01-01T00:00:00Z\n---\n")
	for _, name := range repos {
		versions, _ := s.Registry.Meta.ListVersions(r.Context(), "rubygems", name)
		if len(versions) == 0 {
			continue
		}
		sb.WriteString(name + " " + strings.Join(versions, ",") + " " + strings.Repeat("0", 64) + "\n")
	}
	if base := s.Registry.Upstreams.Sub("rubygems", "index"); base != "" {
		remote := s.Registry.RemoteAt(base)
		if body, err := remote.GetCached(r.Context(), pkrkit.SharedIndexCache(), "/versions"); err == nil {
			if i := strings.Index(body, "---\n"); i >= 0 {
				sb.WriteString(body[i+4:])
			}
		}
	}
	pkrkit.Text(w, http.StatusOK, sb.String(), "text/plain; version=1")
}

func (s *State) compactInfo(w http.ResponseWriter, r *http.Request, name string) {
	name = strings.Trim(name, "/")
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "rubygems", name)
	if len(versions) == 0 {
		if base := s.Registry.Upstreams.Sub("rubygems", "index"); base != "" {
			remote := s.Registry.RemoteAt(base)
			if body, err := remote.GetCached(r.Context(), pkrkit.SharedIndexCache(), "/info/"+pkrkit.URLencode(name)); err == nil {
				pkrkit.Text(w, http.StatusOK, body, "text/plain")
				return
			}
		}
		pkrkit.Text(w, http.StatusOK, "---\n", "text/plain")
		return
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	for _, v := range versions {
		cksum := strings.Repeat("0", 64)
		if art, err := s.Registry.Meta.Get(r.Context(), "rubygems", name, v); err == nil && len(art.Blobs) > 0 {
			cksum = art.Blobs[0].Hex()
		}
		sb.WriteString(v + " |checksum:" + cksum + "\n")
	}
	pkrkit.Text(w, http.StatusOK, sb.String(), "text/plain")
}

func (s *State) specs(w http.ResponseWriter, r *http.Request) {
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "rubygems")
	var tuples [][2]string
	for _, n := range repos {
		versions, _ := s.Registry.Meta.ListVersions(r.Context(), "rubygems", n)
		for _, v := range versions {
			if validGemVersion(v) {
				tuples = append(tuples, [2]string{n, v})
			}
		}
	}
	data := marshalSpecs(tuples)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(data)
	zw.Close()
	pkrkit.OctetResponse(w, gz.Bytes())
}

func validGemVersion(v string) bool {
	if v == "" {
		return false
	}
	c := v[0]
	return c >= '0' && c <= '9'
}

func (s *State) quickMarshal(w http.ResponseWriter, r *http.Request, rel string) {
	rel = strings.TrimPrefix(rel, "Marshal.4.8/")
	stem := strings.TrimSuffix(rel, ".gemspec.rz")
	name, version := nameVersionFromStem(stem)
	if _, err := s.Registry.Meta.Get(r.Context(), "rubygems", name, version); err == nil {
		spec := marshalSpecification(name, version)
		var z bytes.Buffer
		zw := zlib.NewWriter(&z)
		zw.Write(spec)
		zw.Close()
		pkrkit.OctetResponse(w, z.Bytes())
		return
	}
	remote, _ := s.Registry.Remote("rubygems", "")
	if remote != nil {
		if data, err := remote.GetBytes(r.Context(), "/quick/Marshal.4.8/"+rel); err == nil {
			pkrkit.OctetResponse(w, data)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

func (s *State) dependencies(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("gems")
	if q == "" && r.Method == http.MethodPost {
		body, _ := io.ReadAll(r.Body)
		for _, pair := range strings.Split(string(body), "&") {
			kv := strings.SplitN(pair, "=", 2)
			if len(kv) == 2 && kv[0] == "gems" {
				q = kv[1]
			}
		}
	}
	var b []byte = []byte{0x04, 0x08}
	if q == "" {
		b = marshalArrayLen(b, 0)
		pkrkit.OctetResponse(w, b)
		return
	}
	gems := strings.Split(q, ",")
	b = marshalArrayLen(b, len(gems))
	for _, g := range gems {
		if g == "" {
			continue
		}
		name, ver := g, ""
		if i := strings.LastIndex(g, "-"); i >= 0 {
			cand := g[i+1:]
			if cand != "" && cand[0] >= '0' && cand[0] <= '9' {
				ver = cand
				name = g[:i]
			}
		}
		if ver == "" {
			vs, _ := s.Registry.Meta.ListVersions(r.Context(), "rubygems", name)
			ver = pkrkit.HighestVersion(vs)
		}
		b = marshalArrayLen(b, 2)
		b = marshalArrayLen(b, 3)
		b = marshalIstring(b, name)
		b = marshalRequirement(b)
		b = marshalIstring(b, "ruby")
		b = marshalArrayLen(b, 0)
	}
	pkrkit.OctetResponse(w, b)
}

func (s *State) upload(w http.ResponseWriter, r *http.Request) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	data, _ := io.ReadAll(r.Body)
	name, version := extractNameVersionGem(data)
	if name == "" {
		name = "unknown"
	}
	if version == "" {
		version = "0.1.0"
	}
	filename := name + "-" + version + ".gem"
	storeVersionSource(s.Registry, name, version, filename, data, "push", r.Context())
	pkrkit.JSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (s *State) yank(w http.ResponseWriter, r *http.Request, name, version string) {
	if name == "" {
		pkrkit.Error(w, http.StatusNotFound, "missing gem_name")
		return
	}
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	if version == "" {
		vs, _ := s.Registry.Meta.ListVersions(r.Context(), "rubygems", name)
		for _, v := range vs {
			removeVersion(s.Registry, name, v, r.Context())
			s.markYanked(r.Context(), name, v)
		}
	} else {
		removeVersion(s.Registry, name, version, r.Context())
		s.markYanked(r.Context(), name, version)
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

// yankFromRequest reads gem_name/version from the DELETE body (url-encoded
// form) first, then the query string, matching RubyGems client behavior.
func (s *State) yankFromRequest(w http.ResponseWriter, r *http.Request) {
	name, version := "", ""
	if r.Method == http.MethodDelete {
		body, _ := io.ReadAll(r.Body)
		vals, _ := url.ParseQuery(string(body))
		name = vals.Get("gem_name")
		version = vals.Get("version")
	}
	if name == "" {
		name = r.URL.Query().Get("gem_name")
		version = r.URL.Query().Get("version")
	}
	if name == "" {
		name = r.PostFormValue("gem_name")
		version = r.PostFormValue("version")
	}
	s.yank(w, r, name, version)
}

func (s *State) download(w http.ResponseWriter, r *http.Request, filename string) {
	filename = filename[strings.LastIndex(filename, "/")+1:]
	stem := strings.TrimSuffix(filename, ".gem")
	name, version := nameVersionFromStem(stem)
	if art, err := s.Registry.Meta.Get(r.Context(), "rubygems", name, version); err == nil {
		for _, b := range art.Blobs {
			rd, err := s.Registry.Blobs.Open(r.Context(), b.Digest)
			if err != nil || rd == nil {
				continue
			}
			data, _ := io.ReadAll(rd)
			rd.Close()
			pkrkit.OctetResponse(w, data)
			return
		}
	}
	// Pull-through only for gems NOT previously yanked locally. A yank
	// removes the version record, but the upstream mirror still serves it; to
	// keep yanked gems deleted we must NOT fall through to the proxy for a
	// name/version that was yanked. Track yanked names in a meta slot.
	if s.isYanked(r.Context(), name, version) {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	if base := s.Registry.Upstreams.Sub("rubygems", "gems"); base != "" {
		remote := s.Registry.RemoteAt(base)
		if data, err := remote.GetBytes(r.Context(), "/"+filename); err == nil {
			storeVersionSource(s.Registry, name, version, filename, data, "pull", r.Context())
			pkrkit.OctetResponse(w, data)
			return
		}
	}
	pkrkit.Error(w, http.StatusNotFound, "not found")
}

// yankedVersions tracks locally yanked name@version so a delete "sticks".
const yankedMetaKey = "sg-yanked"

func yankedSet(ctx context.Context, reg *pkrkit.Registry) map[string]bool {
	set := map[string]bool{}
	if b, err := reg.Meta.GetMeta(ctx, "rubygems", yankedMetaKey); err == nil {
		_ = json.Unmarshal(b, &set)
	}
	return set
}

func (s *State) isYanked(ctx context.Context, name, version string) bool {
	return yankedSet(ctx, s.Registry)[name+"@"+version]
}

func (s *State) markYanked(ctx context.Context, name, version string) {
	set := yankedSet(ctx, s.Registry)
	set[name+"@"+version] = true
	b, _ := json.Marshal(set)
	_ = s.Registry.Meta.SetMeta(ctx, "rubygems", yankedMetaKey, b)
}

func nameVersionFromStem(stem string) (string, string) {
	if i := strings.LastIndex(stem, "-"); i > 0 {
		return stem[:i], stem[i+1:]
	}
	return stem, "0.0.0"
}

func removeVersion(reg *pkrkit.Registry, name, version string, ctx context.Context) {
	if art, err := reg.Meta.Get(ctx, "rubygems", name, version); err == nil {
		for _, b := range art.Blobs {
			_ = reg.Blobs.Delete(ctx, b.Digest)
		}
	}
	_ = reg.Meta.Delete(ctx, "rubygems", name, version)
}

func storeVersionSource(reg *pkrkit.Registry, name, version, filename string, data []byte, source string, ctx context.Context) {
	art := pkrkit.Artifact{Format: "rubygems", Repository: name, Version: version, Source: source}
	if len(data) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(data)
		digest := "sha256:" + h.SHA256
		if _, err := reg.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(data)); err == nil {
			art.Blobs = append(art.Blobs, pkrkit.Descriptor{Digest: digest, Size: int64(len(data)), Name: filename})
		}
	}
	_ = reg.Meta.Put(ctx, art)
}

// extractNameVersionGem reads metadata.gz (YAML gemspec) from a .gem archive.
func extractNameVersionGem(data []byte) (string, string) {
	tr := tar.NewReader(bytes.NewReader(data))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", ""
		}
		if hdr.Name != "metadata.gz" {
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(tr, 1<<20))
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return "", ""
		}
		ym, _ := io.ReadAll(gz)
		gz.Close()
		return yamlField(string(ym), "name"), yamlVersion(string(ym))
	}
	return "", ""
}

func yamlField(s, field string) string {
	prefix := field + ":"
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(line[len(prefix):])
		}
	}
	for _, line := range strings.Split(s, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func yamlVersion(s string) string {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(trimmed, "version:"); ok {
			v := strings.TrimSpace(rest)
			if v != "" && v[0] >= '0' && v[0] <= '9' {
				return v
			}
		}
	}
	return ""
}

// --- Ruby Marshal 4.8 encoders (byte-exact port of the reference) ----------

func marshalLen(b []byte, n int) []byte {
	if n <= 122 {
		return append(b, byte(n+5))
	}
	var mag []byte
	v := n
	for v > 0 {
		mag = append(mag, byte(v&0xff))
		v >>= 8
	}
	b = append(b, byte(len(mag)))
	return append(b, mag...)
}

func marshalArrayLen(b []byte, n int) []byte {
	out := append(b, 0x5b)
	return marshalLen(out, n)
}

// marshalIstringFirst emits the :E encoding ivar inline (the FIRST string).
func marshalIstringFirst(b []byte, s string) []byte {
	out := append(b, 0x49, 0x22)
	out = marshalLen(out, len(s))
	out = append(out, s...)
	return append(out, 0x06, 0x3a, 0x06, 0x45, 0x54) // :E inline
}

// marshalIstring emits a symbol link (index 0) — subsequent strings.
func marshalIstring(b []byte, s string) []byte {
	out := append(b, 0x49, 0x22)
	out = marshalLen(out, len(s))
	out = append(out, s...)
	return append(out, 0x06, 0x3b, 0x00, 0x54) // :E symbol link 0
}

// marshalVersion wraps a version string in a Gem::Version user-marshal.
func marshalVersion(b []byte, version string) []byte {
	out := append(b, 0x55, 0x3a, 0x11)
	out = append(out, []byte("Gem::Version")...)
	out = append(out, 0x5b, 0x06)
	return marshalIstring(out, version)
}

// marshalRequirement wraps Gem::Requirement user-marshal [">=", Gem::Version("0")].
func marshalRequirement(b []byte) []byte {
	out := append(b, 0x55, 0x3a, 0x15)
	out = append(out, []byte("Gem::Requirement")...)
	out = append(out, 0x5b, 0x06, 0x5b, 0x06, 0x5b, 0x07)
	out = marshalIstring(out, ">=")
	return marshalVersion(out, "0")
}

// marshalFixnum encodes a Ruby fixnum.
func marshalFixnum(b []byte, n int) []byte {
	out := append(b, 0x69)
	if n >= 0 && n <= 122 {
		out = append(out, byte(n+5))
		return out
	}
	var mag []byte
	v := n
	for v > 0 {
		mag = append(mag, byte(v&0xff))
		v >>= 8
	}
	out = append(out, byte(len(mag)))
	return append(out, mag...)
}

// marshalTime encodes a fixed UTC epoch Time ('I' 'u' :Time ... + ivar zone UTC).
func marshalTime(b []byte) []byte {
	out := append(b, 0x49, 0x75, 0x3a, 0x09)
	out = append(out, []byte("Time")...)
	out = append(out, 0x0d, 0x40, 0x00, 0x14, 0xc0, 0x00, 0x00, 0x00, 0x00)
	out = append(out, 0x06, 0x3a, 0x09)
	out = append(out, []byte("zone")...)
	return marshalIstring(out, "UTC")
}

func marshalAuthors(b []byte) []byte {
	out := append(b, 0x5b, 0x06)
	return marshalIstring(out, "")
}

func marshalLicenses(b []byte) []byte {
	out := append(b, 0x5b, 0x06)
	return marshalIstring(out, "MIT")
}

func marshalSpecs(tuples [][2]string) []byte {
	b := []byte{0x04, 0x08}
	b = marshalArrayLen(b, len(tuples))
	for i, t := range tuples {
		b = marshalArrayLen(b, 3)
		if i == 0 {
			b = marshalIstringFirst(b, t[0])
		} else {
			b = marshalIstring(b, t[0])
		}
		b = marshalVersion(b, t[1])
		if i == 0 {
			b = marshalIstringFirst(b, "ruby")
		} else {
			b = marshalIstring(b, "ruby")
		}
	}
	return b
}

// marshalSpecArray builds the 19-element spec array (spec format 4) exactly
// as Ruby's Gem::Specification#_dump expects.
func marshalSpecArray(name, version string) []byte {
	b := []byte{0x04, 0x08}
	b = marshalArrayLen(b, 19)
	b = marshalIstringFirst(b, "4.0.6")       // 0 rubygems_version
	b = marshalFixnum(b, 4)                    // 1 specification_version
	b = marshalIstring(b, name)                // 2 name
	b = marshalVersion(b, version)             // 3 version
	b = marshalTime(b)                         // 4 date
	b = marshalIstring(b, "")                  // 5 summary
	b = marshalRequirement(b)                  // 6 required_ruby_version
	b = marshalRequirement(b)                  // 7 required_rubygems_version
	b = append(b, 0x30)                        // 8 original_platform ("ruby" symbol ref)
	b = marshalArrayLen(b, 0)                  // 9 dependencies []
	b = append(b, 0x30)                        // 10 rubyforge_project
	b = append(b, 0x30)                        // 11 email
	b = marshalAuthors(b)                      // 12 authors
	b = append(b, 0x30)                        // 13 description
	b = append(b, 0x30)                        // 14 homepage
	b = append(b, 0x54)                        // 15 has_rdoc true
	b = marshalIstring(b, "ruby")              // 16 new_platform
	b = marshalLicenses(b)                     // 17 licenses
	b = append(b, 0x7b, 0x00)                  // 18 metadata: empty hash
	return b
}

// marshalSpecification wraps the spec array in a 'u' user-marshal with the
// w_long byte-count length prefix.
func marshalSpecification(name, version string) []byte {
	payload := marshalSpecArray(name, version)
	b := []byte{0x04, 0x08}
	b = append(b, 0x75, 0x3a, 0x17) // 'u' :Gem::Specification
	b = append(b, []byte("Gem::Specification")...)
	mag := []byte{}
	v := len(payload)
	for v > 0 {
		mag = append(mag, byte(v&0xff))
		v >>= 8
	}
	b = append(b, byte(len(mag)))
	b = append(b, mag...)
	b = append(b, payload...)
	return b
}
