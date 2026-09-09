package repoext

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// ---- session resolution cache (tool path) ----

type sessEntry struct {
	org, repo, branch string
	exp                 time.Time
}

type sessCache struct {
	mu  sync.Mutex
	m   map[string]sessEntry
	ttl time.Duration
}

func newSessCache(ttl time.Duration) *sessCache {
	return &sessCache{m: map[string]sessEntry{}, ttl: ttl}
}

func (c *sessCache) get(sid string) (string, string, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[sid]
	if !ok || time.Now().After(e.exp) {
		return "", "", "", false
	}
	return e.org, e.repo, e.branch, true
}

func (c *sessCache) put(sid, org, repo, branch string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[sid] = sessEntry{org: org, repo: repo, branch: branch, exp: time.Now().Add(c.ttl)}
}

func (c *sessCache) evict(sid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, sid)
}

// ---- session triple resolution (tool path, strict) ----

// resolveSession maps a session name to its (org, repo, branch) triple via
// the mapping table. No fallback, no lazy creation: under the eager
// lifecycle-event model every session has its workspace by the time tools
// run. A miss means the event has not been processed yet (retry shortly) or
// the name is not workspace-derived.
func (s *server) resolveSession(ctx context.Context, sid string) (string, string, string, error) {
	if o, r, b, ok := s.cache.get(sid); ok {
		return o, r, b, nil
	}
	row, err := s.store.GetRowBySession(ctx, sid)
	if err != nil {
		return "", "", "", errDownstream("postgres", err)
	}
	if row == nil {
		if _, _, _, ok := parseSession(sid); !ok {
			return "", "", "", errBad("session '%s' does not match org:repo:branch naming; cannot resolve workspace", sid)
		}
		return "", "", "", errNotFound("session '%s' workspace is not ready yet (lifecycle event in progress); retry later", sid)
	}
	s.cache.put(sid, row.Org, row.Repo, row.Branch)
	return row.Org, row.Repo, row.Branch, nil
}

// bindRow records a mapping; a unique conflict means a concurrent path
// already won — converged, not an error.
func (s *server) bindRow(ctx context.Context, org, repo, branch, sid string) error {
	if err := s.store.InsertRow(ctx, org, repo, branch, sid); err != nil {
		if statusOf(err) == 409 {
			return nil
		}
		return err
	}
	return s.store.InsertManaged(ctx, org, repo)
}

// adoptBranch binds an EXISTING branch to a derived session, creating
// the session when needed. Returns (sessionName, adopted). Shared by the
// manual ops surface (completeAdopt) and tool handlers (mr-create target
// resolution).
func (s *server) adoptBranch(ctx context.Context, org, repo, branch string) (string, bool, error) {
	row, err := s.store.GetRow(ctx, org, repo, branch)
	if err != nil {
		return "", false, errDownstream("postgres", err)
	}
	if row != nil {
		return row.SessionName, false, nil
	}
	tree, err := s.lab.GetRepoTree(ctx)
	if err != nil {
		return "", false, err
	}
	if !tree.repoExists(org, repo) {
		return "", false, errNotFound("repository %s/%s does not exist", org, repo)
	}
	if !tree.branchExists(org, repo, branch) {
		return "", false, errNotFound("branch %s/%s#%s does not exist", org, repo, branch)
	}
	name := namingSession(org, repo, branch)
	if err := s.ag.EnsureSession(ctx, name); err != nil {
		return "", false, err
	}
	if err := s.store.InsertRow(ctx, org, repo, branch, name); err != nil {
		return "", false, err
	}
	return name, true, nil
}

// completeAdopt binds an EXISTING branch to a derived session (manual ops
// surface: give an orphan branch a session). Returns (sessionName, adopted).
func (s *server) completeAdopt(ctx context.Context, org, repo, branch string) (string, bool, error) {
	return s.adoptBranch(ctx, org, repo, branch)
}

// ---- HTTP handlers (ops surface; workspace writes are event-driven) ----

// router builds the chi router (shared by main and tests).
func (s *server) router() http.Handler {
	r := chi.NewRouter()
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.health)
		r.Get("/session-map", s.getSessionMap)
		r.Get("/repos", s.listRepos)
		r.Get("/repos/{org}/{repo}/branches", s.listBranches)
		r.Post("/repos/{org}/{repo}/branches/{bm}/session", s.ensureSession)
	})
	return r
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": "repo-extension"})
}

// listRepos: GET /repos — easylab tree annotated with managed flag and session binding.
func (s *server) listRepos(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tree, err := s.lab.GetRepoTree(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	rows, err := s.store.ListRows(ctx)
	if err != nil {
		writeErr(w, errDownstream("postgres", err))
		return
	}
	managed, err := s.store.ListManaged(ctx)
	if err != nil {
		writeErr(w, errDownstream("postgres", err))
		return
	}
	mSet := map[string]bool{}
	for _, m := range managed {
		mSet[m.Org+"/"+m.Repo] = true
	}
	bound := map[string]string{}
	for _, row := range rows {
		bound[row.Org+"/"+row.Repo+"/"+row.Branch] = row.SessionName
	}

	orgs := []interface{}{}
	for org, repos := range tree {
		rl := []interface{}{}
		for repo, bms := range repos {
			bl := []interface{}{}
			for _, bm := range bms {
				var sn interface{}
				if v, ok := bound[org+"/"+repo+"/"+bm.Name]; ok {
					sn = v
				}
				bl = append(bl, map[string]interface{}{"branch": bm.Name, "session_name": sn})
			}
			rl = append(rl, map[string]interface{}{"repo": repo, "managed": mSet[org+"/"+repo], "branches": bl})
		}
		orgs = append(orgs, map[string]interface{}{"org": org, "repos": rl})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"orgs": orgs})
}

// listBranches: GET /repos/{org}/{repo}/branches
func (s *server) listBranches(w http.ResponseWriter, r *http.Request) {
	org, repo := chi.URLParam(r, "org"), chi.URLParam(r, "repo")
	ctx := r.Context()
	tree, err := s.lab.GetRepoTree(ctx)
	if err != nil {
		writeErr(w, err)
		return
	}
	if !tree.repoExists(org, repo) {
		writeErr(w, errNotFound("repository %s/%s does not exist", org, repo))
		return
	}
	rows, err := s.store.ListRowsForRepo(ctx, org, repo)
	if err != nil {
		writeErr(w, errDownstream("postgres", err))
		return
	}
	bound := map[string]string{}
	for _, row := range rows {
		bound[row.Branch] = row.SessionName
	}
	out := []interface{}{}
	for _, bm := range tree[org][repo] {
		var sn interface{}
		if v, ok := bound[bm.Name]; ok {
			sn = v
		}
		out = append(out, map[string]interface{}{"branch": bm.Name, "session_name": sn})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"branches": out})
}

// ensureSession: POST /repos/{org}/{repo}/branches/{bm}/session — bind an
// orphan branch to a (created) session. Idempotent.
func (s *server) ensureSession(w http.ResponseWriter, r *http.Request) {
	org, repo, bm := chi.URLParam(r, "org"), chi.URLParam(r, "repo"), chi.URLParam(r, "bm")
	if !validSessionComponent(bm) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"ok": false, "error": "invalid branch name; cannot derive session"})
		return
	}
	name, adopted, err := s.completeAdopt(r.Context(), org, repo, bm)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "session_name": name, "adopted": adopted,
	})
}

// getSessionMap: GET /session-map?session=NAME — reverse lookup.
func (s *server) getSessionMap(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("session")
	if sid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"ok": false, "error": "session is required"})
		return
	}
	row, err := s.store.GetRowBySession(r.Context(), sid)
	if err != nil {
		writeErr(w, errDownstream("postgres", err))
		return
	}
	if row == nil {
		writeErr(w, errNotFound("session %s has no mapping row", sid))
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"org": row.Org, "repo": row.Repo, "branch": row.Branch,
	})
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	writeJSON(w, statusOf(err), map[string]interface{}{"ok": false, "error": err.Error()})
}
