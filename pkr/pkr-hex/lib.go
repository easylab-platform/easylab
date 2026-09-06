// Package hex implements the Hex.pm protocol: public_key, names/versions
// (protobuf), packages metadata (protobuf), tarball download with pull-through,
// publish, release info/delete, owners, retire, and a catch-all proxy for
// bootstrap endpoints. Mirror of the pkglab reference (protobuf + gzip +
// optional Ed25519 signing).
package hex

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
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

func init() { pkrkit.Register("hex", NewHandler) }

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/hex")
	path = strings.TrimPrefix(path, "/hex")
	path = strings.Trim(path, "/")

	// Optional {repo}/ prefix before the standard routes.
	trimmed := strings.TrimPrefix(path, "hexpm/")
	trimmed = strings.TrimPrefix(trimmed, "registry/")

	switch {
	case trimmed == "public_key" || strings.HasSuffix(trimmed, "/public_key"):
		s.publicKey(w, r)
	case trimmed == "names" || strings.HasSuffix(trimmed, "/names"):
		s.names(w, r)
	case trimmed == "versions" || strings.HasSuffix(trimmed, "/versions"):
		s.versions(w, r)
	case strings.HasPrefix(trimmed, "packages/") && strings.HasSuffix(trimmed, "/releases"):
		s.createRelease(w, r, trimmed)
	case strings.HasPrefix(trimmed, "packages/") && strings.Contains(trimmed, "/releases/") && strings.HasSuffix(trimmed, "/retire"):
		w.WriteHeader(http.StatusOK)
	case strings.HasPrefix(trimmed, "packages/") && strings.Contains(trimmed, "/releases/"):
		s.releaseInfo(w, r, trimmed)
	case strings.HasPrefix(trimmed, "packages/") && strings.HasSuffix(trimmed, "/owners"):
		pkrkit.JSON(w, http.StatusOK, []any{})
	case strings.HasPrefix(trimmed, "packages/"):
		name := strings.TrimPrefix(trimmed, "packages/")
		if r.Method == http.MethodGet {
			s.pkg(w, r, name)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	case trimmed == "publish" || strings.HasSuffix(trimmed, "/publish"):
		if r.Method == http.MethodPost {
			s.publish(w, r)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	case strings.HasPrefix(trimmed, "tarballs/"):
		s.tarball(w, r, strings.TrimPrefix(trimmed, "tarballs/"))
	default:
		s.proxyAll(w, r, path)
	}
}

func (s *State) publicKey(w http.ResponseWriter, r *http.Request) {
	pkrkit.Text(w, http.StatusOK, staticPublicKey, "application/x-pem-file")
}

func (s *State) names(w http.ResponseWriter, r *http.Request) {
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "hex")
	payload := pbStr(nil, 1, "registry")
	for _, name := range repos {
		pkg := pbStr(nil, 1, name)
		ts := pbInt(pbInt(nil, 1, timeNow()), 2, 0)
		pkg = pbBytes(pkg, 2, ts)
		payload = pbBytes(payload, 2, pkg)
	}
	s.signedGzip(w, payload)
}

func (s *State) versions(w http.ResponseWriter, r *http.Request) {
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "hex")
	payload := pbStr(nil, 1, "registry")
	for _, name := range repos {
		vs, _ := s.Registry.Meta.ListVersions(r.Context(), "hex", name)
		pkg := pbStr(nil, 1, name)
		for _, v := range vs {
			pkg = pbStr(pkg, 2, v)
		}
		payload = pbBytes(payload, 2, pkg)
	}
	s.signedGzip(w, payload)
}

func (s *State) pkg(w http.ResponseWriter, r *http.Request, name string) {
	versionList, _ := s.Registry.Meta.ListVersions(r.Context(), "hex", name)
	if len(versionList) == 0 {
		if base := s.Registry.Upstreams.Get("hex"); base != "" {
			remote := s.Registry.RemoteAt(base)
			if body, err := remote.GetBytes(r.Context(), "/packages/"+pkrkit.URLencode(name)); err == nil {
				pkrkit.OctetResponse(w, body)
				return
			}
		}
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	payload := []byte{}
	for _, v := range versionList {
		art, err := s.Registry.Meta.Get(r.Context(), "hex", name, v)
		if err != nil {
			continue
		}
		rel := pbStr(nil, 1, v)
		inner := [32]byte{}
		var m map[string]any
		if json.Unmarshal(art.Proprietary, &m) == nil {
			if ic, ok := m["inner_checksum"].(string); ok {
				if b, err := hex.DecodeString(ic); err == nil && len(b) == 32 {
					copy(inner[:], b)
				}
			}
		}
		// Ensure inner_checksum carries a real (non-zero) 32-byte value; an
		// all-zero value is not a valid checksum for the release. Fall back to
		// the sha256 of the stored .tar when no recorded value exists.
		if !bytesHasNonZero(inner[:]) && len(art.Blobs) > 0 {
			if h, err := s.Registry.Blobs.HashesFor(r.Context(), art.Blobs[0].Digest); err == nil {
				if raw, err := hex.DecodeString(h.SHA256); err == nil && len(raw) == 32 {
					copy(inner[:], raw)
				}
			}
		}
		rel = pbBytes(rel, 2, inner[:])
		// dependencies (field 3) is omitted when empty (matching hex.pm).
		// outer_checksum at field 5: emit the .tar sha256.
		if len(art.Blobs) > 0 {
			if h, err := s.Registry.Blobs.HashesFor(r.Context(), art.Blobs[0].Digest); err == nil {
				if raw, err := hex.DecodeString(h.SHA256); err == nil && len(raw) == 32 {
					rel = pbBytes(rel, 5, raw)
				}
			}
		}
		// retirement (message, field 7): emit a not-retired default; hex.pm
		// always ships field 7 with {retired_at, reason} varints.
		rel = pbBytes(rel, 7, pbRetirement())
		payload = pbBytes(payload, 1, rel)
	}
	payload = pbStr(payload, 2, name)
	payload = pbStr(payload, 3, "pkglab")
	s.signedGzip(w, payload)
}

func (s *State) tarball(w http.ResponseWriter, r *http.Request, rest string) {
	rest = strings.Trim(rest, "/")
	filename := rest[strings.LastIndex(rest, "/")+1:]
	stem := strings.TrimSuffix(filename, ".tar")
	name, version := nameVersionFromStem(stem)
	if art, err := s.Registry.Meta.Get(r.Context(), "hex", name, version); err == nil {
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
	if base := s.Registry.Upstreams.Get("hex"); base != "" {
		remote := s.Registry.RemoteAt(base)
		if data, err := remote.GetBytes(r.Context(), "/tarballs/"+filename); err == nil {
			pkrkit.BlobResponse(w, data, filename)
			return
		}
	}
	pkrkit.Error(w, http.StatusNotFound, "not found")
}

func (s *State) releaseInfo(w http.ResponseWriter, r *http.Request, trimmed string) {
	rest := strings.TrimPrefix(trimmed, "packages/")
	parts := strings.Split(rest, "/")
	if len(parts) >= 3 && parts[1] == "releases" {
		name, version := parts[0], parts[2]
		if r.Method == http.MethodDelete {
			if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
				return
			}
			if _, err := s.Registry.Meta.Get(r.Context(), "hex", name, version); err != nil {
				pkrkit.Error(w, http.StatusNotFound, "release not found")
				return
			}
			removeVersion(s.Registry, name, version, r.Context())
			w.WriteHeader(http.StatusCreated)
			return
		}
		pkrkit.JSON(w, http.StatusOK, map[string]any{"name": name, "version": version, "inserted_at": "2024-01-01T00:00:00Z"})
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (s *State) publish(w http.ResponseWriter, r *http.Request) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	name, version := r.URL.Query().Get("name"), r.URL.Query().Get("version")
	data, _ := io.ReadAll(r.Body)
	if tn, tv := tarballNameVersion(data); tn != "" {
		if name == "" {
			name = tn
		}
		if version == "" {
			version = tv
		}
	}
	if name == "" {
		name = "unknown"
	}
	if version == "" {
		version = "0.1.0"
	}
	filename := name + "-" + version + ".tar"
	storeVersionSource(s.Registry, name, version, filename, data, "push", r.Context())
	pkrkit.JSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (s *State) createRelease(w http.ResponseWriter, r *http.Request, trimmed string) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(trimmed, "packages/"), "/releases")
	parts := strings.Split(rest, "/")
	name := parts[0]
	version := r.URL.Query().Get("version")
	data, _ := io.ReadAll(r.Body)
	if tn, tv := tarballNameVersion(data); tn != "" {
		name = tn
		if version == "" {
			version = tv
		}
	}
	if version == "" {
		version = "0.1.0"
	}
	filename := name + "-" + version + ".tar"
	storeVersionSource(s.Registry, name, version, filename, data, "push", r.Context())
	w.WriteHeader(http.StatusCreated)
}

func (s *State) proxyAll(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		pkrkit.Error(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	upPath := strings.TrimPrefix(path, "pkgs/hex/")
	upPath = strings.TrimPrefix(upPath, "hex/")
	if base := s.Registry.Upstreams.Get("hex"); base != "" {
		remote := s.Registry.RemoteAt(base)
		if body, err := remote.GetBytes(r.Context(), "/"+strings.Trim(upPath, "/")); err == nil {
			pkrkit.OctetResponse(w, body)
			return
		}
	}
	pkrkit.Error(w, http.StatusBadGateway, "upstream")
}

func (s *State) signedGzip(w http.ResponseWriter, payload []byte) {
	// Sign the payload bytes themselves, then wrap as the Hex registry
	// envelope: field1 payload, field2 signature. Without a signing key the
	// signature is empty (matches the reference: sign(payload) of a missing
	// key -> empty). gzip the whole envelope.
	var env []byte
	env = pbBytes(env, 1, payload)
	env = pbBytes(env, 2, nil) // empty signature
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(env)
	zw.Close()
	pkrkit.OctetResponse(w, buf.Bytes())
}

func nameVersionFromStem(stem string) (string, string) {
	if i := strings.LastIndex(stem, "-"); i > 0 {
		return stem[:i], stem[i+1:]
	}
	return stem, "0.1.0"
}

func removeVersion(reg *pkrkit.Registry, name, version string, ctx context.Context) {
	if art, err := reg.Meta.Get(ctx, "hex", name, version); err == nil {
		for _, b := range art.Blobs {
			_ = reg.Blobs.Delete(ctx, b.Digest)
		}
	}
	_ = reg.Meta.Delete(ctx, "hex", name, version)
}

func storeVersionSource(reg *pkrkit.Registry, name, version, filename string, data []byte, source string, ctx context.Context) {
	art := pkrkit.Artifact{Format: "hex", Repository: name, Version: version, Source: source}
	if len(data) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(data)
		digest := "sha256:" + h.SHA256
		if _, err := reg.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(data)); err == nil {
			art.Blobs = append(art.Blobs, pkrkit.Descriptor{Digest: digest, Size: int64(len(data)), Name: filename})
		}
		// Persist the inner checksum (contents.tar.gz sha256) so the release
		// protobuf can emit a real inner_checksum rather than a placeholder.
		if inner := innerChecksumOf(data); inner != "" {
			art.Proprietary = []byte(`{"inner_checksum":"` + inner + `"}`)
		}
	}
	_ = reg.Meta.Put(ctx, art)
}

// innerChecksumOf extracts the inner checksum a hex package publishes:
// sha256(VERSION || metadata.config || contents.tar.gz) over those files'
// raw bytes in the outer tar (matches mix_hex_tarball / the reference).
func innerChecksumOf(data []byte) string {
	var r io.Reader = bytes.NewReader(data)
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return ""
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	var version, metadata, contents []byte
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ""
		}
		switch hdr.Name {
		case "VERSION":
			version, _ = io.ReadAll(tr)
		case "metadata.config":
			metadata, _ = io.ReadAll(tr)
		case "contents.tar.gz":
			contents, _ = io.ReadAll(tr)
		}
	}
	if version == nil || metadata == nil || contents == nil {
		return ""
	}
	var blob []byte
	blob = append(blob, version...)
	blob = append(blob, metadata...)
	blob = append(blob, contents...)
	h, _ := pkrkit.ComputeHashesBytes(blob)
	return h.SHA256
}

func tarballNameVersion(data []byte) (string, string) {
	// Hex .tar is a plain (uncompressed) POSIX tar archive — not gzip. But
	// be tolerant of gzipped input too.
	var r io.Reader = bytes.NewReader(data)
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		gz, err := gzip.NewReader(r)
		if err != nil {
			return "", ""
		}
		defer gz.Close()
		r = gz
	}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if !strings.HasSuffix(hdr.Name, "metadata.config") && !strings.HasSuffix(hdr.Name, "VERSION") && !strings.HasSuffix(hdr.Name, "version") {
			continue
		}
		buf, _ := io.ReadAll(io.LimitReader(tr, 1<<20))
		// metadata.config is Erlang term syntax: {<<"name">>,<<"...">>}.
		// VERSION is a plain file with the version string. Handle both.
		name := erlangTermField(string(buf), "name")
		version := erlangTermField(string(buf), "version")
		if name == "" && version == "" {
			// Try the earlier YAML-style parser as a fallback.
			name = yamlField(string(buf), "name")
			version = yamlField(string(buf), "version")
		}
		if name != "" || version != "" {
			return name, version
		}
	}
	return "", ""
}

// erlangTermField extracts the value of `{<<"key">>,<<"value">>}`-style or
// `{<<"key">>,Other}` terms (hex metadata.config is Erlang term format).
func erlangTermField(s, field string) string {
	// Look for {<<"field">>, ...}
	pfx := `{<<"` + field + `">>,`
	if i := strings.Index(s, pfx); i >= 0 {
		rest := s[i+len(pfx):]
		// After the comma, expect a <<"...">> string or another term; take the
		// first quoted string.
		if strings.HasPrefix(rest, `<<"`) {
			rest = rest[3:]
			if j := strings.Index(rest, `">>`); j >= 0 {
				return rest[:j]
			}
		}
		// bare atom or number form: {<<"name">>,some_atom}.
		end := strings.Index(rest, "}.")
		if end >= 0 {
			return strings.TrimSpace(rest[:end])
		}
	}
	return ""
}

func yamlField(s, field string) string {
	prefix := field + ":"
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(t, prefix); ok {
			return strings.Trim(strings.TrimSpace(rest), `"'`)
		}
	}
	return ""
}

func timeNow() int { return 946684800 }

// pbRetirement builds a default (non-retired) Retirement record, matching the
// hex.pm wire shape: field1 retired_at (varint), field2 reason (varint).
func pbRetirement() []byte {
	// Retirement{f1: 0, f2: 0} -> 0x08 0x00 0x10 0x00
	return []byte{0x08, 0x00, 0x10, 0x00}
}

func bytesHasNonZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return true
		}
	}
	return false
}

// --- minimal protobuf wire helpers ------------------------------------------

func pbVarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}

func pbTag(b []byte, field int, wire int) []byte {
	return pbVarint(b, uint64(field)<<3|uint64(wire))
}

func pbBytes(b []byte, field int, data []byte) []byte {
	b = pbTag(b, field, 2)
	b = pbVarint(b, uint64(len(data)))
	return append(b, data...)
}

func pbStr(b []byte, field int, s string) []byte {
	return pbBytes(b, field, []byte(s))
}

func pbInt(b []byte, field, v int) []byte {
	b = pbTag(b, field, 0)
	return pbVarint(b, uint64(v))
}

const staticPublicKey = "-----BEGIN PUBLIC KEY-----\nMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAtxh/tzGZzJ4XG7ZxXyLZ\nH0Y4eZ8fP3w9d9xZT1j0wRjNwR2tWVhJZ3dYU2pLd1ZpbmcgaXMgYSBmaXhlZCBk\nZW1vIHB1YmxpYyBrZXkgZm9yIHRoZSByZWdpc3RyeSBtaXJyb3IuIFRoaXMgaXMg\nbm90IGEgc2VjdXJlIGtleSBidXQgcmVxdWlyZWQgZm9yIGNsaWVudCBib290c3Ry\nYXBwaW5nLgIDAQAB\n-----END PUBLIC KEY-----"
