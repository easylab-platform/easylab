// Package maven implements the Maven 2 repository protocol (group/artifact/
// version/filename layout, hash sidecars, maven-metadata.xml, PUT/HEAD/DELETE,
// pull-through from Maven Central). Mirror of the pkglab reference.
package maven

import (
	"bytes"
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

func init() { pkrkit.Register("maven", NewHandler) }

type coords struct {
	artifactID string
	version    string
}

func parseMavenPath(p string) (coords, bool) {
	parts := splitPath(strings.Trim(p, "/"))
	if len(parts) < 3 {
		return coords{}, false
	}
	return coords{
		artifactID: parts[len(parts)-3],
		version:    parts[len(parts)-2],
	}, true
}

func (s *State) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/pkgs/maven")
	path = strings.TrimPrefix(path, "/maven")
	path = strings.Trim(path, "/")

	if path == "archetype-catalog.xml" {
		pkrkit.Text(w, http.StatusOK, `<?xml version="1.0" encoding="UTF-8"?><archetype-catalog></archetype-catalog>`, "application/xml")
		return
	}

	switch {
	case strings.HasSuffix(path, ".md5") || strings.HasSuffix(path, ".sha1"):
		if r.Method == http.MethodPut {
			c, _ := parseMavenPath(strings.TrimSuffix(strings.TrimSuffix(path, ".md5"), ".sha1"))
			s.putPath(w, r, path, c)
			return
		}
		s.hashFile(w, r, path)
	case strings.HasSuffix(path, "maven-metadata.xml"):
		if r.Method == http.MethodPut {
			c, ok := parseMavenPath(path)
			if !ok {
				pkrkit.Error(w, http.StatusNotFound, "invalid path")
				return
			}
			s.putPath(w, r, path, coords{c.artifactID, "maven-metadata"})
			return
		}
		s.metadataXML(w, r, path)
	default:
		c, ok := parseMavenPath(path)
		if !ok {
			pkrkit.Error(w, http.StatusNotFound, "invalid maven path")
			return
		}
		switch r.Method {
		case http.MethodPut:
			s.putPath(w, r, path, c)
		case http.MethodHead:
			s.headPath(w, r, path, c)
		case http.MethodGet:
			s.getPath(w, r, path, c)
		case http.MethodDelete:
			if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
				return
			}
			s.deletePath(w, r, path, c)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}
}

func (s *State) getPath(w http.ResponseWriter, r *http.Request, p string, c coords) {
	filename := pathLast(p)
	if art, err := s.Registry.Meta.Get(r.Context(), "maven", c.artifactID, c.version); err == nil {
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
	fetched, err := s.Registry.Fetch(r.Context(), "maven", "", "/"+p)
	if err != nil {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	storeVersionSource(s.Registry, c.artifactID, c.version, filename, fetched.Data, "pull", r.Context())
	pkrkit.BlobResponse(w, fetched.Data, filename)
}

func (s *State) headPath(w http.ResponseWriter, r *http.Request, p string, c coords) {
	filename := pathLast(p)
	if art, err := s.Registry.Meta.Get(r.Context(), "maven", c.artifactID, c.version); err == nil {
		for _, b := range art.Blobs {
			if b.Name == filename {
				w.Header().Set("Content-Length", itoa(b.Size))
				w.WriteHeader(http.StatusOK)
				return
			}
		}
	}
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

func (s *State) putPath(w http.ResponseWriter, r *http.Request, p string, c coords) {
	if !pkrkit.AuthorizeWrite(w, r, s.Auth) {
		return
	}
	filename := pathLast(p)
	data, _ := io.ReadAll(r.Body)
	storeVersionSource(s.Registry, c.artifactID, c.version, filename, data, "push", r.Context())
	pkrkit.JSON(w, http.StatusCreated, map[string]any{"ok": true})
}

func (s *State) deletePath(w http.ResponseWriter, r *http.Request, p string, c coords) {
	filename := pathLast(p)
	if art, err := s.Registry.Meta.Get(r.Context(), "maven", c.artifactID, c.version); err == nil {
		var kept, removed []pkrkit.Descriptor
		for _, b := range art.Blobs {
			if b.Name == filename {
				removed = append(removed, b)
			} else {
				kept = append(kept, b)
			}
		}
		art.Blobs = kept
		if len(kept) == 0 {
			_ = s.Registry.Meta.Delete(r.Context(), "maven", c.artifactID, c.version)
		} else {
			_ = s.Registry.Meta.Put(r.Context(), art)
		}
		for _, b := range removed {
			_ = s.Registry.Blobs.Delete(r.Context(), b.Digest)
		}
	}
	pkrkit.JSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *State) hashFile(w http.ResponseWriter, r *http.Request, p string) {
	isMD5 := strings.HasSuffix(p, ".md5")
	orig := strings.TrimSuffix(strings.TrimSuffix(p, ".md5"), ".sha1")
	c, ok := parseMavenPath(orig)
	if !ok {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	filename := pathLast(orig)
	if art, err := s.Registry.Meta.Get(r.Context(), "maven", c.artifactID, c.version); err == nil {
		for _, b := range art.Blobs {
			if b.Name == filename {
				h, err := s.Registry.Blobs.HashesFor(r.Context(), b.Digest)
				if err == nil {
					val := h.SHA1
					if isMD5 {
						val = h.MD5
					}
					if val != "" {
						pkrkit.Text(w, http.StatusOK, val, "text/plain")
						return
					}
				}
			}
		}
	}
	fetched, err := s.Registry.Fetch(r.Context(), "maven", "", "/"+p)
	if err != nil {
		pkrkit.Error(w, http.StatusNotFound, "not found")
		return
	}
	pkrkit.Text(w, http.StatusOK, string(fetched.Data), "text/plain")
}

func (s *State) metadataXML(w http.ResponseWriter, r *http.Request, p string) {
	clean := strings.Trim(strings.TrimSuffix(p, "maven-metadata.xml"), "/")
	parts := splitPath(clean)
	if len(parts) == 0 || parts[0] == "" {
		pkrkit.Text(w, http.StatusOK, "<metadata></metadata>", "application/xml")
		return
	}
	if len(parts) >= 3 && strings.HasSuffix(parts[len(parts)-1], "-SNAPSHOT") {
		s.versionMetadataXML(w, r, parts)
		return
	}
	artifactID := parts[len(parts)-1]
	groupID := strings.Join(parts[:len(parts)-1], ".")
	versions, _ := s.Registry.Meta.ListVersions(r.Context(), "maven", artifactID)
	pkrkit.SortSemver(versions)
	if len(versions) == 0 {
		pkrkit.Text(w, http.StatusOK, `<?xml version="1.0" encoding="UTF-8"?><metadata><groupId>`+groupID+`</groupId><artifactId>`+artifactID+`</artifactId><versioning><versions></versions></versioning></metadata>`, "application/xml")
		return
	}
	latest := pkrkit.HighestVersion(versions)
	if latest == "" && len(versions) > 0 {
		latest = versions[len(versions)-1]
	}
	// release = highest non-SNAPSHOT version (versions are ascending).
	release := latest
	for _, v := range versions {
		if !strings.Contains(v, "-SNAPSHOT") {
			release = v
		}
	}
	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?><metadata><groupId>` + groupID + `</groupId><artifactId>` + artifactID + `</artifactId><versioning><latest>` + latest + `</latest><release>` + release + `</release><versions>`)
	for _, v := range versions {
		sb.WriteString(`<version>` + v + `</version>`)
	}
	sb.WriteString(`</versions><lastUpdated>20240101120000</lastUpdated></versioning></metadata>`)
	pkrkit.Text(w, http.StatusOK, sb.String(), "application/xml")
}

func (s *State) versionMetadataXML(w http.ResponseWriter, r *http.Request, parts []string) {
	version := parts[len(parts)-1]
	artifactID := parts[len(parts)-2]
	groupID := strings.Join(parts[:len(parts)-2], ".")
	ts, build := "20240101.120000", "1"
	if art, err := s.Registry.Meta.Get(r.Context(), "maven", artifactID, version); err == nil {
		for _, b := range art.Blobs {
			if strings.Contains(b.Name, "-"+version) {
				if v := snapshotTimestampFromName(b.Name); v != "" {
					ts = v
				}
			}
		}
	}
	timestamped := strings.Replace(version, "-SNAPSHOT", "-"+ts+"-"+build, 1)
	sb := `<?xml version="1.0" encoding="UTF-8"?><metadata><groupId>` + groupID + `</groupId><artifactId>` + artifactID + `</artifactId><version>` + version + `</version><versioning><snapshot><timestamp>` + ts + `</timestamp><buildNumber>` + build + `</buildNumber></snapshot><lastUpdated>20240101120000</lastUpdated><snapshotVersions><snapshotVersion><extension>jar</extension><value>` + timestamped + `</value><updated>20240101120000</updated></snapshotVersion><snapshotVersion><extension>pom</extension><value>` + timestamped + `</value><updated>20240101120000</updated></snapshotVersion></snapshotVersions></versioning></metadata>`
	pkrkit.Text(w, http.StatusOK, sb, "application/xml")
}

func snapshotTimestampFromName(name string) string {
	b := []byte(name)
	for i := 0; i+16 <= len(b); i++ {
		if !allDigits(b[i : i+8]) {
			continue
		}
		j := i + 8
		if j >= len(b) || b[j] != '.' {
			continue
		}
		if j+7 > len(b) || !allDigits(b[j+1:j+7]) {
			continue
		}
		j += 7
		if j >= len(b) || b[j] != '-' {
			continue
		}
		j++
		k := j
		for k < len(b) && b[k] >= '0' && b[k] <= '9' {
			k++
		}
		if k == j {
			continue
		}
		return string(b[i:j]) + string(b[j:k])
	}
	return ""
}

func allDigits(b []byte) bool {
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(b) > 0
}

func storeVersionSource(reg *pkrkit.Registry, artifactID, version, filename string, data []byte, source string, ctx context.Context) {
	if version == "" {
		version = "0.0.0"
	}
	art, _ := reg.Meta.Get(ctx, "maven", artifactID, version)
	art.Format = "maven"
	art.Source = source
	art.Repository = artifactID
	art.Version = version
	if len(data) > 0 {
		h, _ := pkrkit.ComputeHashesBytes(data)
		digest := "sha256:" + h.SHA256
		var removed, kept []pkrkit.Descriptor
		for _, b := range art.Blobs {
			if b.Name == filename {
				removed = append(removed, b)
			} else {
				kept = append(kept, b)
			}
		}
		art.Blobs = kept
		if _, err := reg.Blobs.PutIfAbsent(ctx, digest, bytes.NewReader(data)); err == nil {
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

func splitPath(p string) []string {
	if p == "" {
		return []string{}
	}
	return strings.Split(strings.Trim(p, "/"), "/")
}

func pathLast(p string) string {
	parts := splitPath(p)
	if len(parts) == 0 {
		return p
	}
	return parts[len(parts)-1]
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
