// Package generic implements the generic raw-artifact store:
// /{name}/{version}/{filename}. Local-only (no upstream) as in the reference.
package generic

import (
	"context"
	"io"
	"net/http"
	"strconv"
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

func init() { pkrkit.Register("generic", NewHandler) }

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/generic/")
	path = strings.TrimPrefix(path, "generic/")
	path = strings.Trim(path, "/")

	if strings.HasSuffix(path, ".version") {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimSuffix(path, ".version")
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		vs, _ := s.Registry.Meta.ListVersions(r.Context(), "generic", name)
		for _, v := range vs {
			_ = s.Registry.Meta.Delete(r.Context(), "generic", name, v)
		}
		pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}

	parts := strings.Split(path, "/")
	if len(parts) != 3 {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	name, version, filename := parts[0], parts[1], parts[2]

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		art, err := s.Registry.Meta.Get(r.Context(), "generic", name, version)
		if err != nil {
			pkrkit.Error(w, http.StatusNotFound, "not found")
			return
		}
		for _, b := range art.Blobs {
			if b.Name != filename {
				continue
			}
			rd, err := s.Registry.Blobs.Open(r.Context(), b.Digest)
			if err != nil || rd == nil {
				continue
			}
			data, _ := io.ReadAll(rd)
			rd.Close()
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", itoa(len(data)))
				w.WriteHeader(http.StatusOK)
				return
			}
			pkrkit.BlobResponse(w, data, filename)
			return
		}
		pkrkit.Error(w, http.StatusNotFound, "not found")
	case http.MethodPut:
		if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			pkrkit.Error(w, http.StatusBadRequest, "read error")
			return
		}
		store(s.Registry, name, version, filename, data, r.Context())
		pkrkit.JSON(w, http.StatusCreated, map[string]any{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func store(reg *pkrkit.Registry, name, version, filename string, data []byte, ctx context.Context) {
	art, _ := reg.Meta.Get(ctx, "generic", name, version)
	if art.Repository == "" {
		art.Format, art.Repository, art.Version = "generic", name, version
	}
	if len(data) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(data)
		digest := "sha256:" + h.SHA256
		var removed []pkrkit.Descriptor
		var kept []pkrkit.Descriptor
		for _, b := range art.Blobs {
			if b.Name == filename {
				removed = append(removed, b)
			} else {
				kept = append(kept, b)
			}
		}
		art.Blobs = kept
		if _, err := reg.Blobs.PutIfAbsent(ctx, digest, strings.NewReader(string(data))); err == nil {
			art.Blobs = append(art.Blobs, pkrkit.Descriptor{Digest: digest, Size: int64(len(data)), Name: filename})
		}
		for _, b := range removed {
			if b.Digest != digest {
				_ = reg.Blobs.Delete(ctx, b.Digest)
			}
		}
	}
	_ = reg.Meta.Put(ctx, art)
}

func itoa(n int) string { return strconv.Itoa(n) }
