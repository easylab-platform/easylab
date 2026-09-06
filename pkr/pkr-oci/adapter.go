package oci

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/pkr/pkrkit"
)

// OciState configures the OCI adapter.
type OciState struct {
	// Registry is the substrate (metadata + blob + upstreams).
	Registry *pkrkit.Registry
	// Auth is optional; nil disables auth (anonymous pull-only).
	Auth pkrkit.Auth
	// DefaultUpstream is the pull-through base (scheme://host). Empty "https://registry-1.docker.io".
	DefaultUpstream string
	// SelfBase is the external base URL (scheme://host[:port]) used for auth
	// challenge realms and absolute self URIs in responses.
	SelfBase string
}

// Adapter is the OCI protocol handler. It implements http.Handler, so it can
// be mounted at any prefix (usually "/v2").
type Adapter struct {
	state *OciState
	// registryUpstreams is a per-host cache of on-demand upstreams for
	// explicitly prefixed registries (ghcr.io/...).
	registryUpstreamsMu sync.Mutex
	registryUpstreams   map[string]*Upstream
}

// New builds an OCI adapter.
func New(state *OciState) *Adapter {
	if state == nil {
		state = &OciState{}
	}
	if state.DefaultUpstream == "" {
		state.DefaultUpstream = "https://registry-1.docker.io"
	}
	return &Adapter{state: state, registryUpstreams: map[string]*Upstream{}}
}

// Name implements pkrkit.Protocol.
func (a *Adapter) Name() string { return "oci" }

// NewHandler is the constructor signature for pkrkit.Register.
func NewHandler(reg *pkrkit.Registry, cfg map[string]any) (http.Handler, error) {
	state := &OciState{Registry: reg}
	if v, ok := cfg["default_upstream"].(string); ok && v != "" {
		state.DefaultUpstream = v
	}
	if v, ok := cfg["self_base"].(string); ok && v != "" {
		state.SelfBase = v
	}
	if v, ok := cfg["auth"].(pkrkit.Auth); ok {
		state.Auth = v
	}
	return New(state), nil
}

func (a *Adapter) selfBase() string {
	if a.state.SelfBase != "" {
		return strings.TrimSuffix(a.state.SelfBase, "/")
	}
	return "http://localhost:8080"
}

func (a *Adapter) tokenRealm() string {
	base := a.selfBase()
	if strings.HasPrefix(base, "http://") || strings.HasPrefix(base, "https://") {
		return base + "/token"
	}
	return "http://" + base + "/token"
}

// upstreamForRegistry returns the pull-through upstream for a registry host
// ("" = default upstream; docker hub aliases -> default).
func (a *Adapter) upstreamForRegistry(host string) *Upstream {
	host = strings.ToLower(strings.TrimSpace(host))
	switch host {
	case "", "docker.io", "index.docker.io", "registry-1.docker.io":
		return NewUpstream(
			a.state.Registry.Upstreams.ProxyFactory(),
			a.state.DefaultUpstream,
			proxyOf(a.state.Registry.Upstreams, "oci"),
		)
	default:
		a.registryUpstreamsMu.Lock()
		defer a.registryUpstreamsMu.Unlock()
		if u, ok := a.registryUpstreams[host]; ok {
			return u
		}
		u := ForRegistry(
			a.state.Registry.Upstreams.ProxyFactory(),
			host,
			proxyOf(a.state.Registry.Upstreams, host),
		)
		a.registryUpstreams[host] = u
		return u
	}
}

func proxyOf(u *pkrkit.Upstreams, key string) *string {
	if v, ok := u.ProxyURL(key); ok {
		return &v
	}
	return nil
}

// ServeHTTP dispatches every /v2/* path by method.
func (a *Adapter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/v2")
	rel = strings.TrimPrefix(rel, "/")

	switch {
	case rel == "" || rel == "/":
		a.ping(w, r)
	case rel == "_catalog":
		a.catalog(w, r)
	case strings.HasSuffix(rel, "/tags/list"):
		name := strings.TrimSuffix(rel, "/tags/list")
		a.listTags(w, r, name)
	case strings.Contains(rel, "/referrers/"):
		idx := strings.LastIndex(rel, "/referrers/")
		a.listReferrers(w, r, rel[:idx], rel[idx+len("/referrers/"):])
	case strings.Contains(rel, "/manifests/"):
		idx := strings.LastIndex(rel, "/manifests/")
		a.manifest(w, r, rel[:idx], rel[idx+len("/manifests/"):])
	case strings.Contains(rel, "/blobs/uploads/"):
		idx := strings.LastIndex(rel, "/blobs/uploads/")
		a.upload(w, r, rel[:idx], rel[idx+len("/blobs/uploads/"):])
	case strings.Contains(rel, "/blobs/"):
		idx := strings.LastIndex(rel, "/blobs/")
		a.blob(w, r, rel[:idx], rel[idx+len("/blobs/"):])
	default:
		writeJSON(w, http.StatusNotFound, ociError("MANIFEST_UNKNOWN", "manifest unknown"))
	}
}

func (a *Adapter) authorize(r *http.Request, name string, act pkrkit.Action) bool {
	if a.state.Auth == nil {
		return true
	}
	scope := "repository:" + name + ":" + string(act)
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if tok != "" && tok != r.Header.Get("Authorization") {
		if _, ok := a.state.Auth.CheckBearer(r.Context(), tok, scope); ok {
			return true
		}
	}
	if a.state.Auth.Authenticate(r.Context(), r) != "" {
		return true
	}
	return false
}

// challenge emits a Distribution-spec Bearer auth challenge.
func (a *Adapter) challenge(w http.ResponseWriter, name string, act pkrkit.Action) {
	scope := "repository:" + name + ":" + string(act)
	realm := a.tokenRealm()
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s",service="oci-registry",scope="%s"`, realm, scope))
	writeJSON(w, http.StatusUnauthorized, ociError("UNAUTHORIZED", "authentication required"))
}

func (a *Adapter) ping(w http.ResponseWriter, r *http.Request) {
	if a.state.Auth != nil {
		realm := a.tokenRealm()
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s",service="oci-registry"`, realm))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.WriteHeader(http.StatusOK)
}

func (a *Adapter) catalog(w http.ResponseWriter, r *http.Request) {
	repos, _ := a.state.Registry.Meta.ListRepositoriesByFormat(r.Context(), "oci")
	if repos == nil {
		repos = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"repositories": repos})
}

func (a *Adapter) listTags(w http.ResponseWriter, r *http.Request, name string) {
	if !a.authorize(r, name, pkrkit.ActionPull) {
		a.challenge(w, name, pkrkit.ActionPull)
		return
	}
	registry, repo := splitRegistry(name)
	tags, _ := a.state.Registry.Meta.ListVersions(r.Context(), "oci", repo)
	set := map[string]bool{}
	for _, t := range tags {
		set[t] = true
	}
	if up := a.upstreamForRegistry(registry); up != nil {
		if utags, err := up.ListTags(repo); err == nil {
			for _, t := range utags {
				if !set[t] {
					tags = append(tags, t)
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "tags": tags})
}

func (a *Adapter) manifest(w http.ResponseWriter, r *http.Request, name, ref string) {
	if strings.Contains(ref, ":") {
		if _, err := pkrkit.ParseDigest(ref); err != nil {
			writeJSON(w, http.StatusBadRequest, ociError("DIGEST_INVALID", "invalid digest reference"))
			return
		}
	}
	registry, _ := splitRegistry(name)

	switch r.Method {
	case http.MethodHead:
		if !a.authorize(r, name, pkrkit.ActionPull) {
			a.challenge(w, name, pkrkit.ActionPull)
			return
		}
		a.getManifest(w, r, name, ref, false)
	case http.MethodGet:
		if !a.authorize(r, name, pkrkit.ActionPull) {
			a.challenge(w, name, pkrkit.ActionPull)
			return
		}
		a.getManifest(w, r, name, ref, true)
	case http.MethodPut:
		if !a.authorize(r, name, pkrkit.ActionPush) {
			a.challenge(w, name, pkrkit.ActionPush)
			return
		}
		a.putManifest(w, r, name, ref, registry)
	case http.MethodDelete:
		if !a.authorize(r, name, pkrkit.ActionDelete) {
			a.challenge(w, name, pkrkit.ActionDelete)
			return
		}
		if err := a.state.Registry.Meta.Delete(r.Context(), "oci", name, ref); err != nil {
			if pkrkit.IsUnknown(err) {
				writeJSON(w, http.StatusNotFound, ociError("MANIFEST_UNKNOWN", "manifest unknown"))
			} else {
				writeJSON(w, http.StatusInternalServerError, ociError("UNKNOWN", err.Error()))
			}
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *Adapter) getManifest(w http.ResponseWriter, r *http.Request, name, ref string, body bool) {
	registry, repo := splitRegistry(name)
	art, err := a.state.Registry.Meta.Get(r.Context(), "oci", repo, ref)
	if err == nil && len(art.Proprietary) > 0 {
		dgst := art.Digest
		if dgst == "" {
			dgst = "sha256:" + hexDigest(art.Proprietary)
		}
		writeManifest(w, art.Proprietary, art.MediaType, dgst, body)
		return
	}
	// Pull-through.
	up := a.upstreamForRegistry(registry)
	if up != nil {
		if mbody, ct, err := up.GetManifest(repo, ref); err == nil {
			dgst := "sha256:" + hexDigest(mbody)
			blobs := extractBlobs(mbody)
			// Store both the reference and the digest as versions.
			a.state.Registry.Meta.Put(r.Context(), pkrkit.Artifact{
				Format: "oci", Repository: repo, Version: ref,
				MediaType: ct, Proprietary: mbody, Digest: dgst, Blobs: blobs, Source: "pull",
			})
			a.state.Registry.Meta.Put(r.Context(), pkrkit.Artifact{
				Format: "oci", Repository: repo, Version: dgst,
				MediaType: ct, Proprietary: mbody, Digest: dgst, Blobs: blobs, Source: "pull",
			})
			writeManifest(w, mbody, ct, dgst, body)
			return
		}
	}
	writeJSON(w, http.StatusNotFound, ociError("MANIFEST_UNKNOWN", "manifest unknown"))
}

func (a *Adapter) putManifest(w http.ResponseWriter, r *http.Request, name, ref, registry string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ociError("MANIFEST_INVALID", "error reading manifest"))
		return
	}
	mt := r.Header.Get("Content-Type")
	if mt == "" {
		mt = "application/vnd.oci.image.manifest.v1+json"
	}
	sha := "sha256:" + hexDigest(body)
	eff := sha
	if strings.Contains(ref, ":") {
		if _, err := pkrkit.ParseDigest(ref); err != nil {
			writeJSON(w, http.StatusBadRequest, ociError("DIGEST_INVALID", "invalid digest"))
			return
		}
		if !strings.HasPrefix(ref, "sha256:") {
			writeJSON(w, http.StatusBadRequest, ociError("DIGEST_INVALID", "unsupported digest algorithm"))
			return
		}
		if eff != ref {
			writeJSON(w, http.StatusBadRequest, ociError("MANIFEST_INVALID", "manifest digest does not match content"))
			return
		}
	}
	blobs := extractBlobs(body)
	art := pkrkit.Artifact{Format: "oci", Repository: name, Version: ref,
		MediaType: mt, Proprietary: body, Digest: eff, Blobs: blobs, Source: "push"}
	if err := a.state.Registry.Meta.Put(r.Context(), art); err != nil {
		writeJSON(w, http.StatusInternalServerError, ociError("UNKNOWN", err.Error()))
		return
	}
	if eff != ref {
		dg := art
		dg.Version = eff
		a.state.Registry.Meta.Put(r.Context(), dg)
	}
	// OCI 1.1 ?tag= values.
	if q := r.URL.Query().Get("tag"); q != "" {
		tagArt := art
		tagArt.Version = q
		a.state.Registry.Meta.Put(r.Context(), tagArt)
	}
	w.Header().Set("Docker-Content-Digest", eff)
	w.Header().Set("Location", "/v2/"+name+"/manifests/"+eff)
	w.WriteHeader(http.StatusCreated)
}

func (a *Adapter) blob(w http.ResponseWriter, r *http.Request, name, digest string) {
	if _, err := pkrkit.ParseDigest(digest); err != nil {
		writeJSON(w, http.StatusBadRequest, ociError("DIGEST_INVALID", "invalid digest"))
		return
	}
	switch r.Method {
	case http.MethodHead:
		if !a.authorize(r, name, pkrkit.ActionPull) {
			a.challenge(w, name, pkrkit.ActionPull)
			return
		}
		a.checkBlob(w, r, digest)
	case http.MethodGet:
		if !a.authorize(r, name, pkrkit.ActionPull) {
			a.challenge(w, name, pkrkit.ActionPull)
			return
		}
		a.getBlob(w, r, name, digest)
	case http.MethodDelete:
		if !a.authorize(r, name, pkrkit.ActionDelete) {
			a.challenge(w, name, pkrkit.ActionDelete)
			return
		}
		if err := a.state.Registry.Blobs.Delete(r.Context(), digest); err != nil {
			writeJSON(w, http.StatusInternalServerError, ociError("UNKNOWN", err.Error()))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *Adapter) checkBlob(w http.ResponseWriter, r *http.Request, digest string) {
	size, _ := a.state.Registry.Blobs.Stat(r.Context(), digest)
	if size == nil {
		writeJSON(w, http.StatusNotFound, ociError("BLOB_UNKNOWN", "blob unknown to registry"))
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(*size, 10))
	w.Header().Set("Docker-Content-Digest", digest)
	w.WriteHeader(http.StatusOK)
}

func (a *Adapter) getBlob(w http.ResponseWriter, r *http.Request, name, digest string) {
	size, _ := a.state.Registry.Blobs.Stat(r.Context(), digest)
	if size != nil {
		rd, err := a.state.Registry.Blobs.Open(r.Context(), digest)
		if err != nil || rd == nil {
			writeJSON(w, http.StatusNotFound, ociError("BLOB_UNKNOWN", "blob unknown to registry"))
			return
		}
		defer rd.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Accept-Ranges", "bytes")
		if rng := r.Header.Get("Range"); rng != "" {
			start, end, ok := parseRange(rng, *size)
			if !ok {
				w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", *size))
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			if _, err := rd.Seek(start, io.SeekStart); err != nil {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, *size))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			io.CopyN(w, rd, end-start+1)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(*size, 10))
		w.WriteHeader(http.StatusOK)
		io.Copy(w, rd)
		return
	}
	// Pull-through: stream from upstream, caching locally while verifying.
	registry, repo := splitRegistry(name)
	up := a.upstreamForRegistry(registry)
	if up == nil {
		writeJSON(w, http.StatusNotFound, ociError("BLOB_UNKNOWN", "blob unknown to registry"))
		return
	}
	resp, lenp, err := up.GetBlob(repo, digest)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, ociError("UPSTREAM_ERROR", err.Error()))
		return
	}
	defer resp.Body.Close()
	// Buffer through a temp file to verify digest before storing.
	tmpPath, err := writeAndVerify(r.Context(), resp.Body, digest)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, ociError("UPSTREAM_ERROR", err.Error()))
		return
	}
	// Serve the buffered bytes now that they verified.
	data, err := readFile(r.Context(), tmpPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ociError("UNKNOWN", err.Error()))
		return
	}
	a.state.Registry.Blobs.PutIfAbsent(r.Context(), digest, strings.NewReader(string(data)))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Docker-Content-Digest", digest)
	if lenp != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(*lenp, 10))
	}
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func (a *Adapter) upload(w http.ResponseWriter, r *http.Request, name, session string) {
	if !a.authorize(r, name, pkrkit.ActionPush) {
		a.challenge(w, name, pkrkit.ActionPush)
		return
	}
	switch r.Method {
	case http.MethodPost:
		// Mount / single-shot / start session.
		if mnt := r.URL.Query().Get("mount"); mnt != "" {
			if size, _ := a.state.Registry.Blobs.Stat(r.Context(), mnt); size != nil {
				w.Header().Set("Location", "/v2/"+name+"/blobs/"+mnt)
				w.Header().Set("Docker-Content-Digest", mnt)
				w.WriteHeader(http.StatusCreated)
				return
			}
		}
		if dgst := r.URL.Query().Get("digest"); dgst != "" {
			if _, err := pkrkit.ParseDigest(dgst); err != nil {
				writeJSON(w, http.StatusBadRequest, ociError("DIGEST_INVALID", "invalid digest"))
				return
			}
			data, err := io.ReadAll(r.Body)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, ociError("DIGEST_INVALID", "read error"))
				return
			}
			if dgst != "sha256:"+hexDigest(data) {
				writeJSON(w, http.StatusBadRequest, ociError("DIGEST_INVALID", "digest does not match content"))
				return
			}
			a.state.Registry.Blobs.PutIfAbsent(r.Context(), dgst, strings.NewReader(string(data)))
			w.Header().Set("Location", "/v2/"+name+"/blobs/"+dgst)
			w.Header().Set("Docker-Content-Digest", dgst)
			w.WriteHeader(http.StatusCreated)
			return
		}
		// Start a session.
		sessID := "sess-" + a.nextSessionID()
		a.state.Registry.Meta.SaveUpload(r.Context(), pkrkit.UploadRecord{ID: sessID, Format: "oci", Repository: name})
		w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+sessID)
		w.Header().Set("Docker-Upload-UUID", sessID)
		w.WriteHeader(http.StatusAccepted)
	case http.MethodGet:
		u, err := a.state.Registry.Meta.GetUpload(r.Context(), session)
		if err != nil {
			writeJSON(w, http.StatusNotFound, ociError("BLOB_UPLOAD_UNKNOWN", "blob upload unknown"))
			return
		}
		w.Header().Set("Docker-Upload-UUID", u.ID)
		w.Header().Set("Range", fmt.Sprintf("0-%d", max64(u.Bytes-1, 0)))
		w.WriteHeader(http.StatusNoContent)
	case http.MethodPatch:
		// Accumulate chunk bytes in-memory per session.
		if !a.sessionAppend(r.Context(), session, r) {
			writeJSON(w, http.StatusNotFound, ociError("BLOB_UPLOAD_UNKNOWN", "blob upload unknown"))
			return
		}
		w.Header().Set("Docker-Upload-UUID", session)
		w.Header().Set("Range", fmt.Sprintf("0-%d", a.sessionSize(session)-1))
		w.Header().Set("Location", "/v2/"+name+"/blobs/uploads/"+session)
		w.WriteHeader(http.StatusAccepted)
	case http.MethodPut:
		// Final chunk (may carry the remainder of the body) + digest commit.
		if !a.sessionAppend(r.Context(), session, r) && a.sessionSize(session) == 0 {
			writeJSON(w, http.StatusNotFound, ociError("BLOB_UPLOAD_UNKNOWN", "blob upload unknown"))
			return
		}
		dgst := r.URL.Query().Get("digest")
		if dgst == "" {
			writeJSON(w, http.StatusBadRequest, ociError("DIGEST_INVALID", "digest parameter missing"))
			return
		}
		if _, err := pkrkit.ParseDigest(dgst); err != nil {
			writeJSON(w, http.StatusBadRequest, ociError("DIGEST_INVALID", "invalid digest"))
			return
		}
		data := a.sessionBytes(session)
		if dgst != "sha256:"+hexDigest(data) {
			writeJSON(w, http.StatusBadRequest, ociError("DIGEST_INVALID", "digest does not match content"))
			return
		}
		if _, err := a.state.Registry.Blobs.PutIfAbsent(r.Context(), dgst, strings.NewReader(string(data))); err != nil {
			writeJSON(w, http.StatusInternalServerError, ociError("UNKNOWN", err.Error()))
			return
		}
		a.sessionDrop(session)
		a.state.Registry.Meta.DeleteUpload(r.Context(), session)
		w.Header().Set("Location", "/v2/"+name+"/blobs/"+dgst)
		w.Header().Set("Docker-Content-Digest", dgst)
		w.WriteHeader(http.StatusCreated)
	case http.MethodDelete:
		a.sessionDrop(session)
		a.state.Registry.Meta.DeleteUpload(r.Context(), session)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (a *Adapter) listReferrers(w http.ResponseWriter, r *http.Request, name, subject string) {
	if !a.authorize(r, name, pkrkit.ActionPull) {
		a.challenge(w, name, pkrkit.ActionPull)
		return
	}
	filter := r.URL.Query().Get("artifactType")
	var manifestList []map[string]any
	versions, _ := a.state.Registry.Meta.ListVersions(r.Context(), "oci", name)
	for _, v := range versions {
		if _, err := pkrkit.ParseDigest(v); err != nil {
			continue
		}
		art, err := a.state.Registry.Meta.Get(r.Context(), "oci", name, v)
		if err != nil {
			continue
		}
		at := manifestArtifactType(art.Proprietary)
		if at == "" || manifestSubject(art.Proprietary) != subject {
			continue
		}
		if filter != "" && at != filter {
			continue
		}
		manifestList = append(manifestList, map[string]any{
			"mediaType": art.MediaType, "digest": art.Digest, "size": len(art.Proprietary),
			"artifactType": at, "annotations": map[string]any{},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
		"manifests": manifestList,
	})
}

// --- helpers ----------------------------------------------------------------

func writeManifest(w http.ResponseWriter, body []byte, mt, dgst string, withBody bool) {
	w.Header().Set("Content-Type", mt)
	w.Header().Set("Docker-Content-Digest", dgst)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if withBody {
		w.WriteHeader(http.StatusOK)
		w.Write(body)
	} else {
		w.WriteHeader(http.StatusOK)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func ociError(code, msg string) map[string]any {
	return map[string]any{"errors": []any{map[string]any{"code": code, "message": msg, "detail": ""}}}
}

func hexDigest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func parseRange(hdr string, size int64) (int64, int64, bool) {
	if !strings.HasPrefix(hdr, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(hdr, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	if parts[0] == "" {
		// suffix: last N bytes
		n, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, false
		}
		start := size - int64(n)
		if start < 0 {
			start = 0
		}
		return start, size - 1, true
	}
	start, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	end := size - 1
	if parts[1] != "" {
		end, err = strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return 0, 0, false
		}
	}
	if start >= size || start > end {
		return 0, 0, false
	}
	return start, end, true
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// sessionKey is a context key for a generated upload uuid.
type sessionKey string

// nextSessionID returns a unique upload-session id.
func (a *Adapter) nextSessionID() string {
	var b [16]byte
	crandRead(b[:])
	return fmt.Sprintf("%x", b[:])
}

// in-flight upload session buffer (in-memory; a real deployment would page
// to a temp file for large layers, matching the reference Cas + persistence).
var sessionBufs sync.Map

func (a *Adapter) sessionAppend(ctx context.Context, session string, r *http.Request) bool {
	if session == "" {
		return false
	}
	// The session must be registered (POST started it) before appending.
	if _, err := a.state.Registry.Meta.GetUpload(ctx, session); err != nil {
		return false
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return false
	}
	val, _ := sessionBufs.LoadOrStore(session, &[]byte{})
	buf := val.(*[]byte)
	*buf = append(*buf, data...)
	// Persist the running size so a resumed GET reports a correct Range.
	u, _ := a.state.Registry.Meta.GetUpload(ctx, session)
	u.Bytes = int64(len(*buf))
	_ = a.state.Registry.Meta.SaveUpload(ctx, u)
	return true
}

func (a *Adapter) sessionSize(session string) int {
	val, ok := sessionBufs.Load(session)
	if !ok {
		return 0
	}
	return len(*(val.(*[]byte)))
}

func (a *Adapter) sessionBytes(session string) []byte {
	val, ok := sessionBufs.Load(session)
	if !ok {
		return nil
	}
	return *val.(*[]byte)
}

func (a *Adapter) sessionDrop(session string) {
	sessionBufs.Delete(session)
}

var crandRead = func(b []byte) {
	if _, err := rand.Read(b); err != nil {
		msg := []byte("fallback-random-seed")
		copy(b, msg)
	}
}

// writeAndVerify streams an upstream body to a temp file while hashing, only
// returning a valid path when the digest matches (CacheThrough semantics: the
// client is served even on mismatch; nothing is cached on mismatch).
func writeAndVerify(ctx context.Context, body io.Reader, digest string) (string, error) {
	tmp, err := createTemp()
	if err != nil {
		return "", err
	}
	defer func() {
		if r := recover(); r != nil {
			// ignore
		}
	}()
	h := sha256.New()
	w, err := newTempWriter(tmp.path)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(io.MultiWriter(w, h), body); err != nil {
		w.Close()
		return "", err
	}
	w.Close()
	got := "sha256:" + hex.EncodeToString(h.Sum(nil))
	if got != digest {
		return "", fmt.Errorf("digest mismatch: got %s", got)
	}
	return tmp.path, nil
}

func readFile(ctx context.Context, path string) ([]byte, error) {
	return fileRead(path)
}

func createTemp() (*tmpFile, error) {
	f, err := newTempFile()
	if err != nil {
		return nil, err
	}
	return f, nil
}
