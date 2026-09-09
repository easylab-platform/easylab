package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// sandboxCtx is the resolved per-call sandbox context: which workspace, which
// worker pod, and the repo revision the pod is synced to.
type sandboxCtx struct {
	session string // raw session name (or derived legacy label)
	cid     string // container key (k8s label value)
	ws      workspace
}

// workspace is the org/repo/branch triple plus the branch's target commit.
type workspace struct {
	org    string
	repo   string
	branch string
	rev    string // branch head commit id
}

// branchOrDefault returns the branch or "latest" when empty.
func (w workspace) branchOrDefault() string {
	if w.branch == "" {
		return "latest"
	}
	return w.branch
}

type wsCacheEntry struct {
	ws      workspace
	expires time.Time
}

// resolveWorkspace maps a tool call to its workspace via the first-class
// `session_name` envelope field ("org:repo:branch", verified against easylab).
// There is no legacy `_org`/`_repo`/`_branch` fallback: the agent always
// carries the session name, and ops-extension talks to easylab only.
func (s *server) resolveWorkspace(ctx context.Context, args map[string]interface{}, sessionName string) (workspace, string, error) {
	if sessionName == "" {
		return workspace{}, "", fmt.Errorf("missing session context (pass session_name)")
	}
	org, repo, bm, ok := parseSessionName(sessionName)
	if !ok {
		return workspace{}, "", fmt.Errorf(
			"session %q is not named org:repo:branch — cannot resolve its workspace (rename the session)", sessionName)
	}
	ws, err := s.lookupWorkspace(ctx, sessionName, org, repo, bm)
	return ws, sessionName, err
}

// lookupWorkspace resolves the branch head via easylab, with a short
// cache to keep the hot path (every sandbox tool call) free of extra round
// trips while a call still notices branch moves quickly. Expired entries
// are swept on every miss so the map cannot grow unbounded across sessions.
func (s *server) lookupWorkspace(ctx context.Context, sid, org, repo, bm string) (workspace, error) {
	s.wsMu.Lock()
	if e, ok := s.wsCache[sid]; ok && time.Now().Before(e.expires) {
		ws := e.ws
		s.wsMu.Unlock()
		return ws, nil
	}
	s.sweepExpiredLocked()
	s.wsMu.Unlock()

	rev, err := s.easylabBranchHead(ctx, org, repo, bm)
	if err != nil {
		return workspace{}, err
	}
	ws := workspace{org: org, repo: repo, branch: bm, rev: rev}
	s.wsMu.Lock()
	s.wsCache[sid] = wsCacheEntry{ws: ws, expires: time.Now().Add(30 * time.Second)}
	s.wsMu.Unlock()
	return ws, nil
}

// sweepExpiredLocked drops expired cache entries; caller holds wsMu.
func (s *server) sweepExpiredLocked() {
	now := time.Now()
	for k, e := range s.wsCache {
		if !now.Before(e.expires) {
			delete(s.wsCache, k)
		}
	}
}

// invalidateWorkspace drops the cached resolution (e.g. after sandbox-port
// moved the branch) so the next call observes the new head.
func (s *server) invalidateWorkspace(sid string) {
	s.wsMu.Lock()
	delete(s.wsCache, sid)
	s.wsMu.Unlock()
}

// easylabBranchHead fetches a branch's target commit id from easylab.
func (s *server) easylabBranchHead(ctx context.Context, org, repo, bm string) (string, error) {
	branches, err := s.sdk.Branches(ctx, org, repo)
	if err != nil {
		return "", fmt.Errorf("easylab branchs %s/%s: %w", org, repo, err)
	}
	for _, b := range branches {
		if b.GetName() == bm {
			return b.GetSha(), nil
		}
	}
	return "", fmt.Errorf("branch %q not found in %s/%s", bm, org, repo)
}

// ensureSandbox resolves the workspace and requires the session's worker pod
// to already exist (created explicitly via sandbox-create). It does NOT create
// the pod: sandbox-* tools fail with a clear "create first" error when the pod
// is absent, so the agent must explicitly choose a base image. When pod exists
// and needSync, the workspace branch head is synced in (server-side by easylab).

// filterServicesBySession narrows a easylab /ops/services JSON response to the
// services carrying a easylab/session annotation equal to `session`.
func filterServicesBySession(body, session string) string {
	var in struct {
		Services []map[string]interface{} `json:"services"`
	}
	if jerr := json.Unmarshal([]byte(body), &in); jerr != nil || in.Services == nil {
		return ""
	}
	var out []map[string]interface{}
	for _, svc := range in.Services {
		ann, _ := svc["annotations"].(map[string]interface{})
		if s, _ := ann["easylab/session"].(string); s == session {
			out = append(out, svc)
		}
	}
	enc, err := json.Marshal(map[string]interface{}{"services": out})
	if err != nil {
		return ""
	}
	return string(enc)
}

// filterServicesByExtra narrows by tool-layer filters org/repo/kind.
func filterServicesByExtra(body, org, repo, kind string) string {
	var in struct {
		Services []map[string]interface{} `json:"services"`
	}
	if jerr := json.Unmarshal([]byte(body), &in); jerr != nil || in.Services == nil {
		return ""
	}
	var out []map[string]interface{}
	for _, svc := range in.Services {
		if kind != "" {
			if k, _ := svc["kind"].(string); k != kind {
				continue
			}
		}
		if org != "" || repo != "" {
			ann, _ := svc["annotations"].(map[string]interface{})
			if org != "" {
				if o, _ := ann["easylab/org"].(string); o != org {
					continue
				}
			}
			if repo != "" {
				if r, _ := ann["easylab/repo"].(string); r != repo {
					continue
				}
			}
		}
		out = append(out, svc)
	}
	enc, err := json.Marshal(map[string]interface{}{"services": out})
	if err != nil {
		return ""
	}
	return string(enc)
}

// labelKey sanitizes an arbitrary label (session names contain ':' which is
// illegal in k8s label values and pod names) into a deterministic k8s-safe
// key. Values that are already valid are used as-is (e.g. UUIDs).
func labelKey(label string) string {
	if validLabelValue(label) {
		return label
	}
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:])[:16]
}

// validLabelValue follows the k8s label value grammar: alphanumerics, '-',
// '_' and '.', at most 63 chars.
func validLabelValue(v string) bool {
	if len(v) == 0 || len(v) > 63 {
		return false
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// sessionKey derives the k8s-safe sandbox key for a session name (delegates to
// labelKey, which is the canonical derivation).
func sessionKey(session string) string {
	return labelKey(session)
}
