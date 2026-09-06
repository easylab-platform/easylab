// Package webapi is the EasyLab UI-facing aggregate gateway. It fans out to
// the EasyLab core (Lab + ops APIs) and (optionally) the embedded agent
// session backend, presenting a single, UI-friendly surface for the bundled
// SPA and clients. It is separate from the raw /api/v1 Lab surface so the UI
// contract can evolve without touching core semantics.
package webapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"easyvcs/internal/revision"
	"easyvcs/internal/store"
	"easyvcs/internal/webapi/static"
)

var (
	errBadRequest = errors.New("bad request")
	errNotFound   = errors.New("not found")
)

// API aggregates the EasyLab resources for the UI.
type API struct {
	CS *store.CentralStore
	// AgentURL, when set, forwards /sessions|providers|models|presets|config
	// to the embedded agent session backend (zergx-agent SEA). When empty the
	// UI omits chat/session settings.
	AgentURL string
	// Up points back at easylab's own /api/v1 + /v2 so the aggregate surface
	// can fetch real services/catalog/releases (all EasyVCS semantics).
	SelfBase string
	upc      *upstream
}

// upstreamClient returns the upstream client (lazy-initialized to server base).
func (a *API) upstreamClient() *upstream {
	if a.upc == nil {
		a.upc = newUpstream(a.SelfBase)
	}
	return a.upc
}

// MountAPI registers the aggregate API routes onto mux (specific paths so
// they take precedence over the raw /api/v1 Lab facade). The SPA is mounted
// separately via MountSPA.
func (a *API) MountAPI(mux *http.ServeMux) {
	mux.HandleFunc("/api/v1/status", a.status)
	mux.HandleFunc("/api/v1/zergx-config", a.status)
	mux.HandleFunc("/api/v1/repos", a.repos)
	mux.HandleFunc("/api/v1/repos/ensure", a.ensureRepo)
	mux.HandleFunc("/api/v1/repos/ensure-org", a.ensureOrg)
	mux.HandleFunc("/api/v1/repos/fork", a.forkRepo)
	mux.HandleFunc("/api/v1/repos/clone", a.cloneRepo)
	mux.HandleFunc("/api/v1/fs/list", a.fsList)
	mux.HandleFunc("/api/v1/fs/read", a.fsRead)
	if a.AgentURL != "" {
		mux.HandleFunc("/api/v1/sessions", func(w http.ResponseWriter, r *http.Request) { a.proxy(w, r, a.AgentURL+"/api/v1/sessions") })
		mux.HandleFunc("/api/v1/sessions/", func(w http.ResponseWriter, r *http.Request) { a.proxy(w, r, a.AgentURL+"/api/v1/sessions") })
		mux.HandleFunc("/api/v1/providers", func(w http.ResponseWriter, r *http.Request) { a.proxy(w, r, a.AgentURL+"/api/v1/providers") })
		mux.HandleFunc("/api/v1/providers/", func(w http.ResponseWriter, r *http.Request) { a.proxy(w, r, a.AgentURL+"/api/v1/providers") })
		mux.HandleFunc("/api/v1/models", func(w http.ResponseWriter, r *http.Request) { a.proxy(w, r, a.AgentURL+"/api/v1/models") })
		mux.HandleFunc("/api/v1/presets", func(w http.ResponseWriter, r *http.Request) { a.proxy(w, r, a.AgentURL+"/api/v1/presets") })
		mux.HandleFunc("/api/v1/presets/", func(w http.ResponseWriter, r *http.Request) { a.proxy(w, r, a.AgentURL+"/api/v1/presets") })
		mux.HandleFunc("/api/v1/config", func(w http.ResponseWriter, r *http.Request) { a.proxy(w, r, a.AgentURL+"/api/v1/config") })
		mux.HandleFunc("/api/v1/tool-config", func(w http.ResponseWriter, r *http.Request) { a.proxy(w, r, a.AgentURL+"/api/v1/tool-config") })
		mux.HandleFunc("/api/v1/tools", func(w http.ResponseWriter, r *http.Request) { a.proxy(w, r, a.AgentURL+"/api/v1/tools") })
	}
	mux.HandleFunc("/api/v1/containers", a.containers)
	mux.HandleFunc("/api/v1/containers/", a.containers)
	mux.HandleFunc("/api/v1/sandboxes", a.containers)
	mux.HandleFunc("/api/v1/sandboxes/", a.containers)
	mux.HandleFunc("/api/v1/deployments", a.deployments)
	mux.HandleFunc("/api/v1/deployments/", a.deployments)
	mux.HandleFunc("/api/v1/images/build", a.imageBuild)
	mux.HandleFunc("/api/v1/builds/{id}/stream", a.buildStream)
	mux.HandleFunc("/api/v1/packages", a.packages)
	mux.HandleFunc("/api/v1/packages/", a.packages)
}

// MountSPA serves the embedded SPA at "/" with fallback to index.html.
func (a *API) MountSPA(mux *http.ServeMux) {
	spaFS, err := static.FS()
	if err != nil {
		return
	}
	spa := http.FileServer(http.FS(spaFS))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/" || strings.HasPrefix(p, "/assets/") || strings.HasPrefix(p, "/icons/") ||
			strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".css") || strings.HasSuffix(p, ".json") ||
			strings.HasSuffix(p, ".webmanifest") || strings.HasSuffix(p, ".svg") || strings.HasSuffix(p, ".png") {
			spa.ServeHTTP(w, r)
			return
		}
		http.ServeFileFS(w, r, spaFS, "index.html")
	})
}

func (a *API) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "name": "easylab", "version": "0.1.0"})
}

func (a *API) repos(w http.ResponseWriter, r *http.Request) {
	repos, err := a.CS.List()
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, repos)
}

func (a *API) ensureRepo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Org  string `json:"org"`
		Repo string `json:"repo"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Org == "" || body.Repo == "" {
		badReq(w)
		return
	}
	_, err := a.CS.Create(store.RepoRef{Namespace: body.Org, Name: body.Repo})
	if err != nil && !strings.Contains(err.Error(), "exists") {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"org": body.Org, "repo": body.Repo, "ok": true})
}

func (a *API) ensureOrg(w http.ResponseWriter, _ *http.Request) {
	// Namespaces are implicit in the central store; nothing to pre-create.
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) forkRepo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Org     string `json:"org"`
		Repo    string `json:"repo"`
		NewOrg  string `json:"new_org"`
		NewName string `json:"new_name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Org == "" || body.Repo == "" || body.NewName == "" {
		badReq(w)
		return
	}
	dstNS := body.NewOrg
	if dstNS == "" {
		dstNS = body.Org
	}
	_, err := a.CS.Fork(store.RepoRef{Namespace: body.Org, Name: body.Repo}, store.RepoRef{Namespace: dstNS, Name: body.NewName})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) cloneRepo(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "message": "clone via git-pull / import"})
}

// fsList lists a directory at a revision.
func (a *API) fsList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo, ok := a.openRepo(q.Get("org"), q.Get("repo"))
	if !ok {
		badReq(w)
		return
	}
	ws := revision.NewWorkspace(repo)
	treeID, err := treeOfQuery(ws, repo, q.Get("ref"))
	if err != nil {
		writeErr(w, err)
		return
	}
	tree, err := ws.ReadTree(treeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	path := q.Get("path")
	entries := []map[string]any{}
	for _, e := range tree.SortedEntries() {
		if path != "" && path != e.Name {
			continue
		}
		entries = append(entries, map[string]any{
			"name": e.Name, "kind": strings.ToLower(string(e.Kind.String())), "path": e.Name,
		})
	}
	writeJSON(w, map[string]any{"entries": entries})
}

// fsRead reads a file (repo-relative path) at a revision.
func (a *API) fsRead(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	repo, ok := a.openRepo(q.Get("org"), q.Get("repo"))
	if !ok {
		badReq(w)
		return
	}
	ws := revision.NewWorkspace(repo)
	treeID, err := treeOfQuery(ws, repo, q.Get("ref"))
	if err != nil {
		writeErr(w, err)
		return
	}
	tree, err := ws.ReadTree(treeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	entry, err := ws.FindEntry(tree, q.Get("path"))
	if err != nil {
		writeErr(w, errNotFound)
		return
	}
	data, err := ws.ReadBlob(entry.ID)
	if err != nil {
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

func (a *API) containers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	svcs, err := a.upstreamClient().services(ctx)
	if err != nil {
		// easylab backend not reachable in tests/first-boot: degrade to empty.
		writeJSON(w, map[string]any{"containers": []any{}, "sandboxes": []any{}})
		return
	}
	writeJSON(w, map[string]any{"containers": svcs, "sandboxes": svcs})
}

func (a *API) deployments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	svcs, err := a.upstreamClient().services(ctx)
	if err != nil {
		writeJSON(w, map[string]any{"deployments": []any{}})
		return
	}
	// Only deployment-kind services.
	var deploys []map[string]any
	for _, s := range svcs {
		if k, _ := s["kind"].(string); k == "deployment" || k == "" {
			deploys = append(deploys, s)
		}
	}
	writeJSON(w, map[string]any{"deployments": deploys})
}

func (a *API) imageBuild(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	// Forward to easylab /api/v1/ops/builds (buildah). Return its build id.
	code, b, err := a.upstreamClient().post(ctx, "/api/v1/ops/builds", body)
	if err != nil {
		writeErr(w, err)
		return
	}
	if code != http.StatusOK {
		writeErr(w, fmt.Errorf("image build: %d %s", code, string(b)))
		return
	}
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	writeJSON(w, out)
}

// buildStream proxies easylab's build/task event stream (SSE) to the SPA.
func (a *API) buildStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	target := a.upstreamClient().self + "/api/v1/ops/tasks/" + url.PathEscape(id) + "/stream"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writeErr(w, err)
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_ = writeStream(w, resp)
}

func (a *API) packages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Packages surface = OCI catalog (repositories) + Lab releases aggregated
	// across all repo namespaces (EasyVCS semantics: repository/revision/ref).
	var repos []string
	if rr, err := a.upstreamClient().catalog(ctx); err == nil {
		repos = rr
	}
	var rels []map[string]any
	all, _ := a.CS.List()
	for _, rr := range all {
		lst, err := a.upstreamClient().releases(ctx, rr.Namespace, rr.Name)
		if err != nil {
			continue
		}
		for _, l := range lst {
			l["namespace"] = rr.Namespace
			l["repository"] = rr.Name
			rels = append(rels, l)
		}
	}
	writeJSON(w, map[string]any{
		"packages": repos,
		"releases": rels,
	})
}

func (a *API) openRepo(ns, name string) (*store.Repo, bool) {
	if ns == "" || name == "" {
		return nil, false
	}
	repo, err := a.CS.OpenRepo(store.RepoRef{Namespace: ns, Name: name})
	if err != nil {
		return nil, false
	}
	return repo, true
}

func treeOfQuery(ws *revision.Workspace, repo *store.Repo, ref string) (objectID, error) {
	if ref == "" {
		revs, err := ws.Log()
		if err != nil || len(revs) == 0 {
			return objectID{}, nil
		}
		snap, err := repo.GetSnapshot(revs[0].Hash)
		if err != nil {
			return objectID{}, err
		}
		return snap.TreeID, nil
	}
	return resolveTreeID(ws, repo, ref)
}

// objectID is a placeholder to keep signatures tidy; replaced by object.ID.
type objectID = [32]byte

func resolveTreeID(ws *revision.Workspace, repo *store.Repo, ref string) (objectID, error) {
	// Use ResolveRevisions to map ref -> snapshot, then tree.
	ids, rvs, err := ws.ResolveRevisions([]string{ref})
	if err != nil {
		return objectID{}, err
	}
	if len(ids) == 0 || len(rvs) == 0 {
		return objectID{}, nil
	}
	snap, err := repo.GetSnapshot(ids[0])
	if err != nil {
		return objectID{}, err
	}
	return snap.TreeID, nil
}

func badReq(w http.ResponseWriter) {
	w.WriteHeader(http.StatusBadRequest)
	writeJSON(w, map[string]string{"error": "bad request"})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	if err == errBadRequest || err == errNotFound {
		w.WriteHeader(http.StatusBadRequest)
	} else if err == errNotFound {
		w.WriteHeader(http.StatusNotFound)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
	}
	writeJSON(w, map[string]string{"error": err.Error()})
}

// proxy returns an HTTP handler that reverse-proxies r to the target URL,
// preserving method + body and streaming the response (for SSE).
func (a *API) proxyTarget(w http.ResponseWriter, r *http.Request, target string) {
	a.proxy(w, r, target)
}

func (a *API) proxy(w http.ResponseWriter, r *http.Request, target string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	req.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_ = writeStream(w, resp)
}

func writeStream(w http.ResponseWriter, resp *http.Response) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if err != nil {
			return nil
		}
	}
}

var _ = store.RepoRef{}
