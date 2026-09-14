package repoext

import (
	"context"
	"sync"
	"time"
)

// ---- session resolution cache (tool path) ----

type sessEntry struct {
	org, repo, branch string
	exp               time.Time
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
func (s *server) resolveSession(ctx context.Context, tenant, sid string) (string, string, string, error) {
	if tenant == "" {
		tenant = "default"
	}
	if o, r, b, ok := s.cache.get(tenant + "\x00" + sid); ok {
		return o, r, b, nil
	}
	row, err := s.store.GetRowBySession(ctx, tenant, sid)
	if err != nil {
		return "", "", "", errDownstream("postgres", err)
	}
	if row == nil {
		if _, _, _, ok := parseSession(sid); !ok {
			return "", "", "", errBad("session '%s' does not match org:repo:branch naming; cannot resolve workspace", sid)
		}
		return "", "", "", errNotFound("session '%s' workspace is not ready yet (lifecycle event in progress); retry later", sid)
	}
	s.cache.put(tenant+"\x00"+sid, row.Org, row.Repo, row.Branch)
	return row.Org, row.Repo, row.Branch, nil
}

// bindRow records a mapping; a unique conflict means a concurrent path
// already won — converged, not an error.
func (s *server) bindRow(ctx context.Context, tenant, org, repo, branch, sid string) error {
	if err := s.store.InsertRow(ctx, tenant, org, repo, branch, sid); err != nil {
		if statusOf(err) == 409 {
			return nil
		}
		return err
	}
	return s.store.InsertManaged(ctx, tenant, org, repo)
}

// adoptBranch binds an EXISTING branch to a derived session, creating
// the session when needed. Returns (sessionName, adopted). Shared by the
// manual ops surface (completeAdopt) and tool handlers (mr-create target
// resolution).
func (s *server) adoptBranch(ctx context.Context, tenant, org, repo, branch string) (string, bool, error) {
	row, err := s.store.GetRow(ctx, tenant, org, repo, branch)
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
	if err := s.store.InsertRow(ctx, tenant, org, repo, branch, name); err != nil {
		return "", false, err
	}
	return name, true, nil
}

// completeAdopt binds an EXISTING branch to a derived session (manual ops
// surface: give an orphan branch a session). Returns (sessionName, adopted).
func (s *server) completeAdopt(ctx context.Context, tenant, org, repo, branch string) (string, bool, error) {
	return s.adoptBranch(ctx, tenant, org, repo, branch)
}

// ---- HTTP handlers (ops surface; workspace writes are event-driven) ----

// listRepos: GET /repos — easylab tree annotated with managed flag and session binding.

// listBranches: GET /repos/{org}/{repo}/branches

// ensureSession: POST /repos/{org}/{repo}/branches/{bm}/session — bind an
// orphan branch to a (created) session. Idempotent.

// getSessionMap: GET /session-map?session=NAME — reverse lookup.

// ---- helpers ----
