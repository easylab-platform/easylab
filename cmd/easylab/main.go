// Command easylab is the EasyVCS API server.
//
// It exposes change-native operations over HTTP against the central store (the
// same ~/.easyvcs/easyvcs.db used by the CLI). Repositories are addressed by
// namespace and name: /repo/{namespace}/{name}/....
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	_ "github.com/easylab-platform/artifact/cargo"
	_ "github.com/easylab-platform/artifact/composer"
	_ "github.com/easylab-platform/artifact/conan"
	_ "github.com/easylab-platform/artifact/generic"
	_ "github.com/easylab-platform/artifact/go"
	_ "github.com/easylab-platform/artifact/helm"
	_ "github.com/easylab-platform/artifact/hex"
	_ "github.com/easylab-platform/artifact/maven"
	_ "github.com/easylab-platform/artifact/npm"
	_ "github.com/easylab-platform/artifact/nuget"
	_ "github.com/easylab-platform/artifact/oci"
	_ "github.com/easylab-platform/artifact/pub"
	_ "github.com/easylab-platform/artifact/pypi"
	_ "github.com/easylab-platform/artifact/rubygems"
	_ "github.com/easylab-platform/artifact/swiftpm"
	_ "github.com/easylab-platform/artifact/system"
	"github.com/easylab-platform/artifact/core"

	"github.com/abcp-sdk/agent-proto/agent/v1/agentv1connect"
	"github.com/easylab-platform/easylab-proto/easylab/v1/easylabv1connect"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
	"github.com/easylab-platform/easyvcs/transfer"
	"github.com/easylab-platform/easyvcs/webapi"
)

// envOrStr returns env var value or a default.
func envOrStr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

type server struct {
	cs       *store.CentralStore
	tokens   map[string]bool
	registry *pkrkit.Registry
	selfBase string
	ops      *opsState
	ui       *webapi.API
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	selfBase := flag.String("self-base", "", "external base URL (scheme://host[:port]) used for absolute URLs in registry responses. Required so clients (OCI, nuget, ...) reach the server instead of localhost.")
	flag.Parse()

	// Metadata backend is switchable (sqlite default | postgres for cluster).
	cs, err := store.OpenDriver(store.DriverConfig{
		Kind: envOrStr("EASYVCS_DB_DRIVER", store.KindSQLite),
		DSN:  envOrStr("EASYVCS_DB_DSN", ""),
	})
	if err != nil {
		log.Fatal("open store:", err)
	}
	_ = cs.SetWAL()
	reg, err := openRegistry(store.HomeDir())
	if err != nil {
		log.Fatal("open registry:", err)
	}
	if *selfBase == "" {
		*selfBase = os.Getenv("EASYVCS_SELF_BASE")
	}
	opsState, err := newOpsState()
	if err != nil {
		log.Fatal("init ops:", err)
	}
	s := &server{cs: cs, tokens: buildTokenSet(), registry: reg, selfBase: strings.TrimSuffix(*selfBase, "/"), ops: opsState}

	// Start the background mirror scheduler (push on-change, pull on-interval).
	go s.runMirrorLoop(context.Background())

	mux := s.router()
	_ = mux

	log.Printf("easylab listening on %s (db %s)", *addr, store.DBPath())
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// buildTokenSet collects acceptable bearer tokens from the EASYVCS_TOKEN env
// var (comma-separated). Tokens are compared in constant time to resist timing
// attacks.
func buildTokenSet() map[string]bool {
	set := map[string]bool{}
	if env := os.Getenv("EASYVCS_TOKEN"); env != "" {
		for _, t := range strings.Split(env, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				set[t] = true
			}
		}
	}
	return set
}

// requireAuth wraps a handler so that it is denied (401) unless a valid bearer
// token is present. Read-only endpoints are intentionally left unauthenticated
// (see router).
func (s *server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authOK(r) {
			writeErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized: missing or invalid token"))
			return
		}
		next(w, r)
	}
}

func (s *server) authOK(r *http.Request) bool {
	if len(s.tokens) == 0 {
		// No tokens configured: auth is disabled (open server).
		return true
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return false
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if token == "" {
		return false
	}
	for valid := range s.tokens {
		if subtle.ConstantTimeCompare([]byte(token), []byte(valid)) == 1 {
			return true
		}
	}
	return false
}

// router builds the HTTP routing table. It is shared by main and tests so
// PathValue is populated correctly (tests must route through the mux).
func (s *server) router() *http.ServeMux {
	mux := http.NewServeMux()

	// Repository lifecycle (write) requires auth when tokens are configured.
	mux.HandleFunc("POST /repositories/{ns}/{name}", s.requireAuth(s.handleCreateRepo))
	mux.HandleFunc("GET /repositories", s.handleListRepos)
	mux.HandleFunc("DELETE /repositories/{ns}/{name}", s.requireAuth(s.handleDeleteRepo))

	// Smart protocol: advertise/fetch are read-only (no auth); push is auth.
	mux.HandleFunc("POST /repo/{ns}/{name}/advertise", s.handleAdvertise)
	mux.HandleFunc("POST /repo/{ns}/{name}/fetch", s.handleFetch)
	mux.HandleFunc("POST /repo/{ns}/{name}/push", s.requireAuth(s.handlePush))

	// Write endpoints require auth.
	mux.HandleFunc("POST /repo/{ns}/{name}/commit", s.requireAuth(s.handleCommit))
	mux.HandleFunc("POST /repo/{ns}/{name}/rebase", s.requireAuth(s.handleRebase))
	mux.HandleFunc("POST /repo/{ns}/{name}/merge", s.requireAuth(s.handleMerge))
	mux.HandleFunc("POST /repo/{ns}/{name}/squash", s.requireAuth(s.handleSquash))
	mux.HandleFunc("POST /repo/{ns}/{name}/ref/{ref}", s.requireAuth(s.handleSetRef))
	mux.HandleFunc("DELETE /repo/{ns}/{name}/ref/{ref}", s.requireAuth(s.handleDeleteRef))

	// Read-only endpoints are unauthenticated.
	mux.HandleFunc("GET /repo/{ns}/{name}/log", s.handleLog)
	mux.HandleFunc("GET /repo/{ns}/{name}/refs", s.handleListRefs)
	mux.HandleFunc("GET /repo/{ns}/{name}/diff/{a}/{b}", s.handleDiff)
	mux.HandleFunc("GET /repo/{ns}/{name}/revision/{id}", s.handleChange)

	// UI-facing aggregate gateway (SPA + unified API). Mounted under "/" and
	// /api/v1 (facade); core Lab/ops APIs live under their own paths below.
	ui := &webapi.API{CS: s.cs, AgentURL: os.Getenv("EASYLAB_AGENT_URL"), SelfBase: "http://127.0.0.1:18160"}
	// UI aggregate API paths (specific) + SPA at "/".
	ui.MountAPI(mux)
	ui.MountSPA(mux)
	s.ui = ui

	// Lab (hosting) API under /api/v1. The Lab mux owns the full /api/v1
	// subtree and populates its own PathValue fields from its patterns.
	mux.Handle("/api/v1/", s.labRouter())

	// Ops: dev/deploy platform (runs, tasks, builds).
	s.mountOps(mux)

	// Package registry (pkrkit): OCI /v2 + all language protocols.
	s.mountPackageRegistry(mux)

	// Typed Connect contract surface. These are the strong-typed RPC endpoints
	// consumed by the Flutter client and the ext servers. They mount under
	// /easylab.v1 and /agent.v1 (Connect/ gRPC-compatible).
	mux.Handle(easylabv1connect.NewLabServiceHandler(&connLab{s}))
	mux.Handle(easylabv1connect.NewOpsServiceHandler(&connOps{s}))
	mux.Handle(easylabv1connect.NewRegistryServiceHandler(&connRegistry{s}))

	// agent.v1 gateway: forwards to the real agent backend. Web/flutter talk to
	// easylab (single entry); ext servers connect to the agent directly.
	if agentURL := os.Getenv("EASYLAB_AGENT_URL"); agentURL != "" {
		ca := newConnAgent(agentURL, os.Getenv("EASYLAB_AGENT_TOKEN"))
		mux.Handle(agentv1connect.NewAgentServiceHandler(ca))
	}

	return mux
}

// mountOps wires the /ops subtree onto the top-level mux.
func (s *server) mountOps(mux *http.ServeMux) {
	if s.ops == nil {
		return
	}
	sub := s.opsRouter()
	// The ops router uses full /api/v1/... patterns; register each directly on
	// the top-level mux so PathValue is populated by the outer request.
	mux.Handle("/api/v1/ops/namespaces", http.HandlerFunc(s.opsNamespaces))
	mux.Handle("/api/v1/ops/tasks", http.HandlerFunc(s.opsTasksList))
	mux.Handle("/api/v1/ops/tasks/{id}", http.HandlerFunc(s.opsTaskGet))
	mux.Handle("/api/v1/ops/tasks/{id}/stream", http.HandlerFunc(s.opsTaskStream))
	mux.Handle("/api/v1/ops/builds", http.HandlerFunc(s.opsBuild))
	// Service launch (host docker engine socket backend).
	mux.Handle("/api/v1/ops/services", http.HandlerFunc(s.opsServicesList))
	mux.Handle("POST /api/v1/ops/services", http.HandlerFunc(s.opsServiceLaunch))
	mux.Handle("/api/v1/ops/services/{name}", http.HandlerFunc(s.opsServiceGet))
	mux.Handle("DELETE /api/v1/ops/services/{name}", http.HandlerFunc(s.opsServiceDelete))
	mux.Handle("POST /api/v1/ops/services/{name}/scale", http.HandlerFunc(s.opsServiceScale))
	// Sandbox pass-through: run commands / read / write files inside a
	// persistent podman service container (no repo commit; used by easyvcs-ops).
	mux.Handle("POST /api/v1/ops/sandbox/{name}/exec", http.HandlerFunc(s.opsSandboxExec))
	mux.Handle("GET /api/v1/ops/sandbox/{name}/file", http.HandlerFunc(s.opsSandboxReadFile))
	mux.Handle("PUT /api/v1/ops/sandbox/{name}/file", http.HandlerFunc(s.opsSandboxWriteFile))
	_ = sub
}

// mountPackageRegistry mounts every enabled pkrkit protocol. The generic
// protocol (raw artifacts, used by Lab releases) is always mounted; the OCI
// registry is mounted at /v2 and each language protocol at /pkgs/<name>.
//
// A labTokenAuth is always supplied so write authentication is decided live
// against the store: when the instance is open (no registered users/tokens)
// anonymous write is permitted, otherwise a valid write-level token is
// required. This mirrors the Lab's labPrincipal policy.
func (s *server) mountPackageRegistry(mux *http.ServeMux) {
	if s.registry == nil {
		return
	}
	reg := s.registry
	auth := &labTokenAuth{cs: s.cs, tokens: s.tokens}
	for _, name := range pkrkit.Registered() {
		// Build per-protocol config with the correct self_base: OCI uses the
		// origin root (its /token realm), everything else prefixes /pkgs/<name>.
		cfg := map[string]any{"auth": auth}
		if s.selfBase != "" {
			if name == "oci" {
				cfg["self_base"] = s.selfBase
			} else {
				cfg["self_base"] = s.selfBase + "/pkgs/" + name
			}
		}
		h, err := pkrkit.Build(name, reg, cfg)
		if err != nil {
			log.Printf("registry: skip %s: %v", name, err)
			continue
		}
		// OCI is special-cased to /v2; the rest mount under /pkgs/<name>.
		if name == "oci" {
			// The OCI Bearer challenge points at a /token realm; expose it.
			tokenH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				s.serveOCIToken(w, r, auth)
			})
			mux.Handle("/v2/token", tokenH)
			mux.Handle("/token", tokenH)
			mux.Handle("/v2", h)
			mux.Handle("/v2/", h)
		} else {
			mux.Handle("/pkgs/"+name+"/", h)
			mux.Handle("/pkgs/"+name, h)
		}
	}
}

func (s *server) serveOCIToken(w http.ResponseWriter, r *http.Request, auth pkrkit.Auth) {
	scopes := []string{}
	for _, v := range r.URL.Query()["scope"] {
		for _, f := range strings.Fields(v) {
			if f != "" {
				scopes = append(scopes, f)
			}
		}
	}
	username := auth.Authenticate(r.Context(), r)
	canWrite := false
	for _, sc := range scopes {
		if strings.HasSuffix(sc, ":push") || strings.HasSuffix(sc, ":delete") {
			canWrite = true
		}
	}
	if username == "" && canWrite {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"errors": []any{
			map[string]any{"code": "UNAUTHORIZED", "message": "authentication required"},
		}})
		return
	}
	tok := auth.IssueToken(r.Context(), username, scopes, 3600)
	writeJSON(w, http.StatusOK, map[string]any{"token": tok, "access_token": tok, "expires_in": 3600})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func repoRef(w http.ResponseWriter, r *http.Request) (*store.Repo, bool) {
	// Unused helper stub kept for interface clarity.
	return nil, false
}

func (s *server) handleCreateRepo(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	_, err := s.cs.Create(store.RepoRef{Namespace: ns, Name: name})
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"repo": ns + "/" + name, "created": "true"})
}

func (s *server) handleListRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := s.cs.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, repos)
}

func (s *server) handleDeleteRepo(w http.ResponseWriter, r *http.Request) {
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	if err := s.cs.Delete(store.RepoRef{Namespace: ns, Name: name}); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": ns + "/" + name})
}

func (s *server) repo(w http.ResponseWriter, r *http.Request) (*store.Repo, bool) {
	repo, err := s.cs.OpenRepo(store.RepoRef{Namespace: r.PathValue("ns"), Name: r.PathValue("name")})
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return nil, false
	}
	return repo, true
}

func (s *server) handleCommit(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	req, err := decodeJSONBody[commitReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	author := store.Author{Name: "easyvcs", Email: "easyvcs@example.com"}
	if req.Author != nil {
		author = *req.Author
	}

	// Two commit modes:
	//  - tree_id provided: classic tree commit.
	//  - changes provided: atomic file-change commit (parent_hash + changes).
	if len(req.Changes) > 0 {
		var parentID object.ID
		if req.ParentHash != "" {
			var err error
			parentID, err = object.HexToID(req.ParentHash)
			if err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
		}
		var changes []revision.FileChangeSpec
		for _, c := range req.Changes {
			changes = append(changes, revision.FileChangeSpec{
				Path:    c.Path,
				Content: []byte(c.Content),
				Delete:  c.Delete,
			})
		}
		snap, ch, err := revision.NewWorkspace(repo).CommitFromChanges(
			parentID, changes, req.Description, author, req.RevisionID,
		)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"revision_id": ch.ID,
			"snapshot":    snap.RevisionHash.String(),
			"tree":        snap.TreeID.String(),
		})
		return
	}

	// Classic tree commit path.
	treeID, err := object.HexToID(req.TreeID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var parents []object.ID
	for _, p := range req.Parents {
		id, err := object.HexToID(p)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		parents = append(parents, id)
	}
	snap, ch, err := revision.NewWorkspace(repo).Commit(revision.CommitParams{
		RevisionID:  req.RevisionID,
		Parents:     parents,
		TreeID:      treeID,
		Description: req.Description,
		Author:      author,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"revision_id": ch.ID,
		"snapshot":    snap.RevisionHash.String(),
		"tree":        snap.TreeID.String(),
	})
}

type changeItem struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	Delete  bool   `json:"delete,omitempty"`
}

type commitReq struct {
	RevisionID  string        `json:"revision_id,omitempty"`
	Parents     []string      `json:"parents,omitempty"`
	TreeID      string        `json:"tree_id,omitempty"`
	ParentHash  string        `json:"parent_hash,omitempty"`
	Changes     []changeItem  `json:"changes,omitempty"`
	Description string        `json:"description"`
	Author      *store.Author `json:"author,omitempty"`
	// Ref is the branch the write lands on (default: repo default branch).
	// Every write must resolve to a branch.
	Ref string `json:"ref,omitempty"`
	// NewCommit, when true, creates a fresh revision instead of amending the
	// branch tip. Default (false) amends onto the branch's current revision.
	NewCommit bool `json:"new_commit,omitempty"`
}

type rebaseReq struct {
	RevisionID string   `json:"revision_id"`
	NewParents []string `json:"new_parents"`
}

func (s *server) handleRebase(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	req, err := decodeJSONBody[rebaseReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	var parents []object.ID
	for _, p := range req.NewParents {
		id, err := object.HexToID(p)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		parents = append(parents, id)
	}
	snap, ch, err := revision.NewWorkspace(repo).Rebase(req.RevisionID, parents)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"revision_id": ch.ID,
		"snapshot":    snap.RevisionHash.String(),
	})
}

type mergeReq struct {
	Base   string `json:"base"`
	Ours   string `json:"ours"`
	Theirs string `json:"theirs"`
}

func (s *server) handleMerge(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	req, err := decodeJSONBody[mergeReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	base, _ := object.HexToID(req.Base)
	ours, _ := object.HexToID(req.Ours)
	theirs, _ := object.HexToID(req.Theirs)
	mergedID, atoms, err := revision.NewWorkspace(repo).Merge(base, ours, theirs)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"merged_tree": mergedID.String(),
		"conflicts":   atoms,
	})
}

type squashReq struct {
	RevisionID string `json:"revision_id"`
}

func (s *server) handleSquash(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	req, err := decodeJSONBody[squashReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	snap, ch, err := revision.NewWorkspace(repo).Squash(req.RevisionID)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"revision_id": ch.ID,
		"snapshot":    snap.RevisionHash.String(),
	})
}

type setRefReq struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
}

func (s *server) handleSetRef(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	name := r.PathValue("ref")
	req, err := decodeJSONBody[setRefReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	kind := store.RefKind(req.Kind)
	if kind != store.RefBranch && kind != store.RefTag {
		kind = store.RefBranch
	}
	ref, err := revision.NewWorkspace(repo).SetRef(name, kind, req.Target)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, ref)
}

func (s *server) handleDeleteRef(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	name := r.PathValue("ref")
	if err := revision.NewWorkspace(repo).DeleteRef(name); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
}

func (s *server) handleListRefs(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	refs, err := revision.NewWorkspace(repo).ListRefs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, refs)
}

func (s *server) handleLog(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	changes, err := revision.NewWorkspace(repo).Log()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	type entry struct {
		RevisionID  string `json:"revision_id"`
		Snapshot    string `json:"snapshot"`
		Tree        string `json:"tree"`
		Description string `json:"description"`
	}
	var out []entry
	for _, ch := range changes {
		snap, err := repo.GetSnapshot(ch.Hash)
		if err != nil {
			continue
		}
		out = append(out, entry{
			RevisionID:  ch.ID,
			Snapshot:    snap.RevisionHash.String(),
			Tree:        snap.TreeID.String(),
			Description: snap.Description,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleChange(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	ch, err := repo.GetRevision(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	snap, err := repo.GetSnapshot(ch.Hash)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"revision_id": ch.ID,
		"snapshot":    snap.RevisionHash.String(),
		"tree":        snap.TreeID.String(),
		"description": snap.Description,
	})
}

func (s *server) handleDiff(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	a, err := object.HexToID(r.PathValue("a"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	b, err := object.HexToID(r.PathValue("b"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ws := revision.NewWorkspace(repo)
	aTree := snapshotOrTree(ws, a)
	bTree := snapshotOrTree(ws, b)
	changes, err := ws.Diff(aTree, bTree)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, changes)
}

func snapshotOrTree(ws *revision.Workspace, id object.ID) object.ID {
	if snap, err := ws.GetSnapshot(id); err == nil {
		return snap.TreeID
	}
	return id
}

// ---- Smart protocol endpoints ----

type advertiseReq struct {
	Have        []string `json:"have"`         // change ids this client already has
	HaveObjects []string `json:"have_objects"` // object ids this client already owns
	WantChanges []string `json:"want_changes"` // optional: only fetch these + ancestors
}

type advertiseResp struct {
	Repo    string       `json:"repo"`
	Changes []string     `json:"changes"` // change ids available on the server
	Refs    []*store.Ref `json:"refs"`    // refs available on the server
}

// handleAdvertise tells a client which change ids and refs the server holds, so
// the client can compute a delta for fetch.
func (s *server) handleAdvertise(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	changes, err := repo.ListRevisions()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	refs, err := repo.ListRefs()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var ids []string
	for _, c := range changes {
		ids = append(ids, c.ID)
	}
	writeJSON(w, http.StatusOK, advertiseResp{Repo: repo.String(), Changes: ids, Refs: refs})
}

// handleFetch returns a Bundle for all changes the client does NOT already
// have (delta based on `have`), plus all refs. The client also reports which
// object ids it already owns (haveObjects), so the server only sends objects
// the client lacks. This implements a true object-level want/have: shared
// objects are not re-transmitted.
func (s *server) handleFetch(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	req, err := decodeJSONBody[advertiseReq](r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// Objects are global and deduplicated, so we only need to know whether the
	// client already owns each object id.
	haveObj := map[string]bool{}
	for _, o := range req.HaveObjects {
		haveObj[o] = true
	}
	haveSet := map[string]bool{}
	for _, c := range req.Have {
		haveSet[c] = true
	}
	b, err := transfer.Collect(repo, req.Have, func(id object.ID) bool {
		return haveObj[id.String()]
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// Selective fetch: if the client requested specific changes (e.g. a branch
	// tip), filter the bundle to those changes and their ancestors.
	if len(req.WantChanges) > 0 {
		wanted := changeSetWithAncestors(repo, req.WantChanges, haveSet)
		b = filterBundle(b, wanted)
	}
	// Return the bundle as a compressed binary frame for efficiency.
	writeBundle(w, b)
}

// writeBundle serializes a bundle to the binary frame and writes it as an
// octet-stream response (gzip-compressed). Consumers that need JSON can still
// decode the frame.
func writeBundle(w http.ResponseWriter, b *transfer.Bundle) {
	encoded, err := transfer.CompressBundle(b)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(encoded)
}

// changeSetWithAncestors returns the set of change ids that are either the
// requested changes or any of their ancestors. `have` marks changes the client
// already owns (excluded from the result).
func changeSetWithAncestors(repo *store.Repo, wants []string, have map[string]bool) map[string]bool {
	result := map[string]bool{}
	// Load all changes once.
	changes, err := repo.ListRevisions()
	if err != nil {
		return result
	}
	byID := map[string]*store.Revision{}
	for _, c := range changes {
		byID[c.ID] = c
	}
	// DFS from each wanted change, walking parents. A parent is referenced by
	// snapshot; we map a snapshot back to the change that owns it.
	snapOwner := map[string]string{}
	for _, c := range changes {
		snapOwner[c.Hash.String()] = c.ID
	}
	var visit func(id string)
	visit = func(id string) {
		if have[id] || result[id] {
			return
		}
		result[id] = true
		ch := byID[id]
		if ch == nil {
			return
		}
		snap, err := repo.GetSnapshot(ch.Hash)
		if err != nil {
			return
		}
		for _, p := range snap.Parents {
			if owner, ok := snapOwner[p.String()]; ok {
				visit(owner)
			}
		}
	}
	for _, w := range wants {
		visit(w)
	}
	return result
}

// filterBundle keeps only the changes in `wanted` (and their snapshots/objects),
// dropping everything else. Objects referenced by remaining snapshots are
// retained; unrelated changes are removed.
func filterBundle(b *transfer.Bundle, wanted map[string]bool) *transfer.Bundle {
	if len(wanted) == 0 {
		return b
	}
	keptChanges := make([]*store.Revision, 0, len(b.Changes))
	keptSnapshots := make([]*store.Snapshot, 0, len(b.Snapshots))
	keptSnapIDs := map[string]bool{}
	keptChangeIDs := map[string]bool{}
	// Index snapshots by change id so we can drop orphans.
	snapByChange := map[string]*store.Snapshot{}
	for _, sn := range b.Snapshots {
		snapByChange[sn.RevisionID] = sn
	}
	for _, ch := range b.Changes {
		if !wanted[ch.ID] {
			continue
		}
		keptChanges = append(keptChanges, ch)
		keptChangeIDs[ch.ID] = true
		if sn, ok := snapByChange[ch.ID]; ok {
			keptSnapshots = append(keptSnapshots, sn)
			keptSnapIDs[sn.RevisionHash.String()] = true
		}
	}
	// Objects: retraverse trees. Rather than reconstruct exact reachability (the
	// bundle already collected objects for ALL changes), we drop objects that
	// belong to dropped changes by keeping only those referenced by kept trees.
	// For simplicity, we keep ALL objects (they're content-addressed and
	// idempotent); selective object pruning is an optimization we can add later.
	out := &transfer.Bundle{Version: b.Version, Repo: b.Repo, Objects: b.Objects}
	_ = keptSnapIDs
	_ = keptChangeIDs
	out.Changes = keptChanges
	out.Snapshots = keptSnapshots
	out.Refs = b.Refs
	return out
}

// decodeJSONBody decodes a JSON request body, transparently decompressing gzip
// when the client set Content-Encoding: gzip.
func decodeJSONBody[T any](r *http.Request) (T, error) {
	var zero T
	reader := r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			return zero, err
		}
		defer gr.Close()
		reader = gr
	}
	var out T
	if err := json.NewDecoder(reader).Decode(&out); err != nil {
		return zero, err
	}
	return out, nil
}

// decodeBundleBody decodes a transfer.Bundle from a potentially gzip-compressed
// request body. It handles both the binary frame and legacy JSON bundles.
func decodeBundleBody(r *http.Request) (*transfer.Bundle, error) {
	reader := r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gr, err := gzip.NewReader(r.Body)
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		reader = gr
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	// Binary frame starts with the bundle magic; otherwise decode as JSON.
	if bytes.HasPrefix(data, []byte("EVCSBUN")) {
		return transfer.UnmarshalBinary(data)
	}
	var b transfer.Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, err
	}
	return &b, nil
}

// handlePush receives a Bundle from a client and applies it to the server's
// repository. It returns the number of changes written.
func (s *server) handlePush(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repo(w, r)
	if !ok {
		return
	}
	b, err := decodeBundleBody(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	n, err := transfer.Apply(repo, b)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"applied": n, "repo": repo.String()})
}
