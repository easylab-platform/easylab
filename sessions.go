package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"connectrpc.com/connect"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	easylabsdk "github.com/easylab-platform/easylab-sdk-go"
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
func (s *server) ensureSandbox(ctx context.Context, args map[string]interface{}, sessionName string, needSync bool) (sandboxCtx, error) {
	ws, sid, err := s.resolveWorkspace(ctx, args, sessionName)
	if err != nil {
		return sandboxCtx{}, err
	}
	info, err := s.workerInfo(ctx, labelKey(sid))
	if err != nil {
		// A missing service = pod not created yet.
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "not found") {
			return sandboxCtx{}, fmt.Errorf("sandbox not created — call sandbox-create with an image first")
		}
		return sandboxCtx{}, fmt.Errorf("inspect sandbox for %s: %w", sid, err)
	}
	s.publishSandboxVars(ctx, sid, info)
	sc := sandboxCtx{session: sid, cid: info.ContainerID, ws: ws}
	if needSync {
		if err := s.ensureSynced(ctx, sc.cid, sc.session, ws); err != nil {
			return sandboxCtx{}, err
		}
	}
	return sc, nil
}

// sandboxWorkerPort is the port worker-go serves inside the sandbox pod.
// The worker's default is 8080, so the ensure request pins WORKER_PORT to keep
// the process, the declared containerPort and the readiness probe in sync.
const sandboxWorkerPort = 48080

// createWorker explicitly creates the session's worker pod from a chosen base
// image. When a pod already exists for the session it is reused (get-or-create)
// so sandbox-create is idempotent; the base image is carried as the
// `easylab/sandbox.image` annotation which easylab uses to derive the worker image.
func (s *server) createWorker(ctx context.Context, sid, baseImage string) (ContainerInfo, error) {
	key := labelKey(sid)
	_, err := s.sdk.LaunchServiceFull(ctx, easylabsdk.LaunchServiceSpec{
		Name:  key,
		Image: baseImage,
		Kind:  "bare",
		Ports: []*easylabv1.PortSpec{{Container: sandboxWorkerPort, Service: 80}},
		Env: map[string]string{
			"WORKER_PORT": "48080",
		},
		Annotations: map[string]string{
			"easylab/session":       sid,
			"easylab/sandbox.image": baseImage,
		},
		Namespace: s.runtimeNamespace,
	})
	if err != nil {
		return ContainerInfo{}, err
	}
	info, err := s.workerInfo(ctx, key)
	if err != nil {
		return ContainerInfo{}, err
	}
	info.SessionName = sid
	return info, nil
}

// statusFromReady maps easylab's readiness into the legacy status vocabulary.
func statusFromInfo(st *easylabv1.ServiceInfo) string {
	if st.GetReady() > 0 {
		return "running"
	}
	if st.GetPhase() != "" {
		return strings.ToLower(st.GetPhase())
	}
	return "pending"
}

// workerURLFrom derives the direct pod worker base.
func workerURLFrom(st *easylabv1.ServiceInfo, port int32) string {
	if st.GetPodIp() == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", st.GetPodIp(), port)
}

// workerInfo fetches the current sandbox state from easylab.
func (s *server) workerInfo(ctx context.Context, key string) (ContainerInfo, error) {
	res, err := s.sdk.GetService(ctx, key)
	if err != nil {
		return ContainerInfo{}, err
	}
	st := res.GetService()
	return ContainerInfo{
		ContainerID: key,
		PodName:     "sandbox-" + key[:8],
		Namespace:   s.runtimeNamespace,
		WorkerURL:   workerURLFrom(st, sandboxWorkerPort),
		PodIP:       st.GetPodIp(),
		Status:      statusFromInfo(st),
	}, nil
}

// destroyWorker deletes the sandbox through easylab. The ID may be the raw
// session name, the derived key, or a pod/short name.
func (s *server) destroyWorker(ctx context.Context, id string) error {
	if _, err := s.sdk.Ops.DeleteService(ctx, connect.NewRequest(&easylabv1.DeleteServiceRequest{Name: labelKey(id)})); err != nil {
		return err
	}
	return nil
}

// ensureSynced pushes the repo tree at ws.rev into the worker unless easylab
// already holds that rev. The tarball fetch + overlay extract happen inside
// easylab (which owns both the repo store and the pod); ops-extension just
// names the target.
func (s *server) ensureSynced(ctx context.Context, cid, session string, ws workspace) error {
	if s.syncedRev(cid) == ws.rev {
		return nil
	}
	if _, err := s.sdk.Sync(ctx, labelKey(session), ws.org, ws.repo, ws.rev, "", false); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	s.setSyncedRev(cid, ws.rev)
	return nil
}

// markUnsynced forgets the tracked rev (worker restart / need_sync) so the
// next ensureSynced pushes again.
func (s *server) markUnsynced(cid string) {
	s.syncMu.Lock()
	delete(s.synced, cid)
	s.syncMu.Unlock()
}

func (s *server) syncedRev(cid string) string {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	return s.synced[cid]
}

func (s *server) setSyncedRev(cid, rev string) {
	s.syncMu.Lock()
	s.synced[cid] = rev
	s.syncMu.Unlock()
}

// ── local naming helpers (moved from internal/k8s; sessions are addressed by
// deterministic keys, no stored state) ──

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

// ContainerInfo is the ops-extension view of a sandbox worker (now sourced
// from easylab instead of client-go pod listings).
type ContainerInfo struct {
	ContainerID string
	PodName     string
	Namespace   string
	WorkerURL   string
	PodIP       string
	Status      string
	SessionName string
}

// filterServicesBySession narrows a easylab /ops/services JSON response to the
// services carrying a easylab/session annotation equal to `session` (opaque
// metadata we own at the tools layer; easylab never interprets it).
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

// filterServicesByExtra narrows a easylab /ops/services JSON response by the
// tool-layer filters org/repo/kind (read from each service's annotations).
// It runs client-side over the already-fetched list, mirroring
// filterServicesBySession — easylab list returns the annotations verbatim.
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
