// Package helm implements the Helm chart repository protocol (index.yaml with
// local entries merged + upstream overlay, chart download with pull-through,
// chart upload) mirroring the pkglab reference.
package helm

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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

func init() { pkrkit.Register("helm", NewHandler) }

func (s *State) base() string {
	if s.SelfBase == "" {
		return "http://localhost:8080/pkgs/helm"
	}
	return strings.TrimSuffix(s.SelfBase, "/")
}

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/helm")
	path = strings.TrimPrefix(path, "/helm")
	path = strings.Trim(path, "/")

	switch {
	case path == "index.yaml":
		if s.indexYaml(w, r) {
			return
		}
		pkrkit.Error(w, http.StatusNotFound, "not found")
	case strings.HasPrefix(path, "charts/"):
		s.chart(w, r, strings.TrimPrefix(path, "charts/"))
	case path == "api/charts":
		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			s.upload(w, r)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	case strings.HasPrefix(path, "api/charts/"):
		s.deleteChart(w, r, strings.TrimPrefix(path, "api/charts/"))
	default:
		pkrkit.Error(w, http.StatusNotFound, "not found")
	}
}

func (s *State) indexYaml(w http.ResponseWriter, r *http.Request) bool {
	var out strings.Builder
	out.WriteString("apiVersion: v1\nentries:\n")
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "helm")
	for _, name := range repos {
		versions, _ := s.Registry.Meta.ListVersions(r.Context(), "helm", name)
		if len(versions) == 0 {
			continue
		}
		out.WriteString("  " + name + ":\n")
		for _, v := range versions {
			art, err := s.Registry.Meta.Get(r.Context(), "helm", name, v)
			if err != nil {
				continue
			}
			fname, digest := name+"-"+v+".tgz", ""
			if len(art.Blobs) > 0 {
				fname, digest = art.Blobs[0].Name, art.Blobs[0].Hex()
			}
			out.WriteString("    - apiVersion: v2\n      name: " + name + "\n      version: " + v +
				"\n      urls:\n        - " + s.base() + "/charts/" + fname +
				"\n      created: 2024-01-01T00:00:00Z\n      digest: " + digest + "\n")
		}
	}
	// Merge upstream index so public charts pull through.
	if base := s.Registry.Upstreams.Get("helm"); base != "" {
		remote := s.Registry.RemoteAt(base)
		if body, err := remote.GetBytes(r.Context(), "/index.yaml"); err == nil {
			text := string(body)
			if idx := strings.Index(text, "entries:"); idx >= 0 {
				rewritten := strings.ReplaceAll(text[idx+len("entries:"):],
					"https://charts.helm.sh/stable/packages/", s.base()+"/charts/")
				out.WriteString(rewritten)
			}
		}
	}
	pkrkit.Text(w, http.StatusOK, out.String(), "application/yaml")
	return true
}

func (s *State) chart(w http.ResponseWriter, r *http.Request, filename string) {
	repos, _ := s.Registry.Meta.ListRepositoriesByFormat(r.Context(), "helm")
	for _, name := range repos {
		versions, _ := s.Registry.Meta.ListVersions(r.Context(), "helm", name)
		for _, v := range versions {
			art, err := s.Registry.Meta.Get(r.Context(), "helm", name, v)
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
	}
	// Pull-through.
	remote, _ := s.Registry.Remote("helm", "")
	if remote != nil {
		if body, err := remote.GetBytes(r.Context(), "/packages/"+filename); err == nil {
			pkrkit.BlobResponse(w, body, filename)
			return
		}
	}
	pkrkit.Error(w, http.StatusNotFound, "chart not found")
}

func (s *State) upload(w http.ResponseWriter, r *http.Request) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	data, _ := io.ReadAll(r.Body)
	ct := r.Header.Get("Content-Type")
	var fname string
	if strings.HasPrefix(ct, "multipart/form-data") {
		fname, data, _ = pkrkit.ExtractFirstFile(data, ct)
	}
	if len(data) == 0 {
		data = bodyOf(r)
	}
	name, version := queryPair(r.URL.Query(), "name"), queryPair(r.URL.Query(), "version")
	if name == "" || version == "" {
		if cn, cv, ok := chartNameVersion(data); ok {
			if name == "" {
				name = cn
			}
			if version == "" {
				version = cv
			}
		}
	}
	if name == "" {
		name = "unknown"
	}
	if version == "" {
		version = "0.1.0"
	}
	if fname == "" {
		fname = name + "-" + version + ".tgz"
	}
	storeChart(s.Registry, name, version, fname, data, r.Context())
	pkrkit.JSON(w, http.StatusCreated, map[string]any{"saved": true})
}

func (s *State) deleteChart(w http.ResponseWriter, r *http.Request, rest string) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	parts := strings.Split(rest, "/")
	name := parts[0]
	if len(parts) >= 2 {
		removeVersion(s.Registry, name, parts[1], r.Context())
	} else {
		vs, _ := s.Registry.Meta.ListVersions(r.Context(), "helm", name)
		for _, v := range vs {
			removeVersion(s.Registry, name, v, r.Context())
		}
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func storeChart(reg *pkrkit.Registry, name, version, filename string, data []byte, ctx context.Context) {
	art := pkrkit.Artifact{Format: "helm", Repository: name, Version: version}
	if len(data) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(data)
		digest := "sha256:" + h.SHA256
		if _, err := reg.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(data)); err == nil {
			art.Blobs = append(art.Blobs, pkrkit.Descriptor{Digest: digest, Size: int64(len(data)), Name: filename})
		}
	}
	_ = reg.Meta.Put(ctx, art)
}

func removeVersion(reg *pkrkit.Registry, name, version string, ctx context.Context) {
	if art, err := reg.Meta.Get(ctx, "helm", name, version); err == nil {
		for _, b := range art.Blobs {
			_ = reg.Blobs.Delete(ctx, b.Digest)
		}
	}
	_ = reg.Meta.Delete(ctx, "helm", name, version)
}

// chartNameVersion reads Chart.yaml (name/version) out of a chart .tgz.
func chartNameVersion(data []byte) (string, string, bool) {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return "", "", false
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", "", false
		}
		if !strings.HasSuffix(hdr.Name, "Chart.yaml") {
			continue
		}
		buf, _ := io.ReadAll(io.LimitReader(tr, 1<<20))
		return yamlScalar(buf, "name"), yamlScalar(buf, "version"), true
	}
	return "", "", false
}

func yamlScalar(data []byte, key string) string {
	prefix := key + ":"
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			return strings.Trim(strings.TrimSpace(rest), `"'`)
		}
	}
	return ""
}

func queryPair(v url.Values, key string) string {
	val := v.Get(key)
	val = strings.ReplaceAll(val, "%20", " ")
	return strings.ReplaceAll(val, "+", " ")
}

func bodyOf(r *http.Request) []byte {
	b, _ := io.ReadAll(r.Body)
	return b
}
