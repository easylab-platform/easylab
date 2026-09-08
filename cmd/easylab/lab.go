package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/easylab-platform/artifact/core"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// labResponse is the envelope for the Lab REST API responses where a top-level
// Ok field is convenient. Most endpoints return the raw payload directly.
type labResponse map[string]any

func labJSON(w http.ResponseWriter, code int, v any) {
	writeJSON(w, code, v)
}

func labErr(w http.ResponseWriter, code int, err error) {
	writeErr(w, code, err)
}

// labRouter returns the /api/v1 subtree of the Lab hosting API. It shares the
// server's central store and token realm.
func (s *server) labRouter() *http.ServeMux {
	m := http.NewServeMux()

	// ---- Users & tokens ----
	m.HandleFunc("GET /api/v1/users", s.labListUsers)
	m.HandleFunc("POST /api/v1/users", s.labCreateUser)
	m.HandleFunc("GET /api/v1/users/{username}", s.labGetUser)
	m.HandleFunc("GET /api/v1/user", s.labCurrentUser)
	m.HandleFunc("POST /api/v1/users/{username}/tokens", s.labCreateToken)
	m.HandleFunc("GET /api/v1/users/{username}/tokens", s.labListTokens)
	m.HandleFunc("DELETE /api/v1/tokens/{token}", s.labDeleteToken)

	// ---- Namespace membership ----
	m.HandleFunc("POST /api/v1/namespaces/{namespace}/members", s.labAddMember)
	m.HandleFunc("GET /api/v1/namespaces/{namespace}/members", s.labListMembers)
	m.HandleFunc("DELETE /api/v1/namespaces/{namespace}/members/{username}", s.labRemoveMember)

	// ---- Repository lifecycle ----
	m.HandleFunc("GET /api/v1/repo", s.labListRepos)
	m.HandleFunc("POST /api/v1/repo", s.labCreateRepo)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}", s.labGetRepo)
	m.HandleFunc("PATCH /api/v1/repo/{namespace}/{repo}", s.labUpdateRepo)
	m.HandleFunc("DELETE /api/v1/repo/{namespace}/{repo}", s.labDeleteRepo)

	// ---- Revisions (commits) ----
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/revisions", s.labListRevisions)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/revisions/{rev}", s.labGetRevision)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/revisions/{rev}/diff", s.labRevisionDiff)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/revisions/{rev}/files", s.labRevisionFiles)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/revisions/{rev}/rebase", s.labRebase)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/rebase-many", s.labRebaseMany)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/revisions/{rev}/drop", s.labDrop)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/revisions/{rev}/revert", s.labRevert)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/revisions/{rev}/squash", s.labSquash)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/revisions/{rev}/resolve", s.labResolve)

	// ---- Content & search ----
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/compare", s.labCompare)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/tree", s.labTree)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/blob", s.labBlob)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/contents/{path...}", s.labContents)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/blame", s.labBlame)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/search", s.labSearch)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/conflicts", s.labConflicts)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/history", s.labHistory)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/status", s.labStatus)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/graph", s.labGraph)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/archive/tarball/{ref}", s.labArchive)

	// ---- Refs (branchs & tags) ----
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/branches", s.labListBranches)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/branches", s.labSetBranch)
	m.HandleFunc("DELETE /api/v1/repo/{namespace}/{repo}/branches/{name}", s.labDeleteBranch)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/tags", s.labListTags)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/tags", s.labSetTag)
	m.HandleFunc("DELETE /api/v1/repo/{namespace}/{repo}/tags/{name}", s.labDeleteTag)

	// ---- Write / files: unified single- or multi-file write that lands on a
	// branch. Default amends onto the branch tip; new_commit creates a fresh
	// revision. "files" is the canonical write spelling.
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/files", s.labWriteFiles)

	// ---- Fork ----
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/fork", s.labFork)

	// ---- Releases ----
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/releases", s.labListReleases)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/releases", s.labCreateRelease)
	m.HandleFunc("DELETE /api/v1/repo/{namespace}/{repo}/releases/{tag}", s.labDeleteRelease)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/releases/{tag}/assets", s.labUploadReleaseAsset)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/releases/{tag}/assets", s.labListReleaseAssets)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/releases/{tag}/assets/{asset}", s.labDownloadReleaseAsset)

	// ---- Merge requests ----
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/merge_requests", s.labListMRs)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/merge_requests", s.labCreateMR)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/merge_requests/{iid}", s.labGetMR)
	m.HandleFunc("PUT /api/v1/repo/{namespace}/{repo}/merge_requests/{iid}", s.labUpdateMR)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/merge_requests/{iid}/merge", s.labMergeMR)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/merge_requests/{iid}/reviews", s.labAddReview)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/merge_requests/{iid}/reviews", s.labListReviews)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/merge_requests/{iid}/comments", s.labAddComment)
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/merge_requests/{iid}/comments", s.labListComments)

	// ---- Mirrors (push-mirror CRUD for normal repos; pull = read-only repo) ----
	m.HandleFunc("GET /api/v1/repo/{namespace}/{repo}/mirrors", s.labListPushMirrors)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/mirrors", s.labCreatePushMirror)
	m.HandleFunc("DELETE /api/v1/repo/{namespace}/{repo}/mirrors/{name}", s.labDeletePushMirror)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/mirrors/{name}/push", s.labPushMirror)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/mirrors/{name}/pull", s.labPullMirror)
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/mirrors/{name}/sync", s.labSyncMirror)

	// Mirror repo manual pull (read-only external source refresh).
	m.HandleFunc("POST /api/v1/repo/{namespace}/{repo}/mirror/pull", s.labMirrorRepoPull)
	return m
}

// ---- auth helpers ----

// labPrincipal resolves the caller from the bearer token. It returns the user
// (nil for anonymous) and an access level. When no users/tokens exist at all
// the server behaves as an open Lab (anonymous is treated as admin) so a fresh
// fixture works without a bootstrap token.
func (s *server) labPrincipal(r *http.Request) (*store.User, string) {
	header := r.Header.Get("Authorization")
	if strings.HasPrefix(header, "Bearer ") {
		token := strings.TrimPrefix(header, "Bearer ")
		if tok, err := s.cs.LookupToken(token); err == nil {
			u, _ := s.cs.GetUser(tok.UserID)
			return u, tok.Level
		}
		// Legacy flat-token realm (pre-Lab tokens).
		if s.tokens != nil && s.tokens[token] {
			return nil, "write"
		}
	}
	if !s.labHasAuth() {
		// No authentication in the Lab realm: open access for read; write also
		// allowed when the server has no token records at all (dev/fixture).
		return nil, "write"
	}
	return nil, "read"
}

// labHasAuth reports whether any users/tokens are registered or a legacy token
// set is configured, which gates whether anonymous write is permitted.
func (s *server) labHasAuth() bool {
	if s.tokens != nil && len(s.tokens) > 0 {
		return true
	}
	users, err := s.cs.ListUsers()
	return err == nil && len(users) > 0
}

// labAdmin wraps a handler requiring an authenticated (non-anonymous) principal
// with write access.
func (s *server) labRequireWrite(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.requireWriteAuth(w, r, func() {
			// Mirror repositories are read-only: reject any write on them.
			if s.writeTargetsMirror(r) {
				labErr(w, http.StatusForbidden, fmt.Errorf("mirror repository is read-only"))
				return
			}
			next(w, r)
		})
	}
}

// labRequireWriteMirrorControl is like labRequireWrite but permits targeting a
// mirror repository, for control-plane operations such as refreshing a read-only
// mirror from its external source.
func (s *server) labRequireWriteMirrorControl(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.requireWriteAuth(w, r, func() { next(w, r) })
	}
}

// requireWriteAuth validates the principal and calls fn on success.
func (s *server) requireWriteAuth(w http.ResponseWriter, r *http.Request, fn func()) {
	u, level := s.labPrincipal(r)
	if u == nil && level != "write" {
		labErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized"))
		return
	}
	if u != nil && level != "write" && level != "admin" {
		labErr(w, http.StatusForbidden, fmt.Errorf("forbidden: token has read-only level"))
		return
	}
	fn()
}

// writeTargetsMirror reports whether the request path references a mirror
// repository (namespace + repo) at a content sub-path that must be read-only.
// Repository lifecycle operations (DELETE the repo, PATCH its hosting meta) are
// still allowed on a mirror; only edits to revision content are forbidden.
func (s *server) writeTargetsMirror(r *http.Request) bool {
	ns := r.PathValue("namespace")
	name := r.PathValue("repo")
	if ns == "" {
		ns = r.PathValue("ns")
	}
	if name == "" {
		name = r.PathValue("name")
	}
	if ns == "" || name == "" {
		return false
	}
	// Repo-root lifecycle ops (DELETE /repositories/{ns}/{name},
	// PATCH /repositories/{ns}/{name}) are allowed on mirrors too.
	if !strings.HasPrefix(r.URL.Path, "/api/v1/repo/"+ns+"/"+name+"/") {
		return false
	}
	repo, err := s.cs.OpenRepo(store.RepoRef{Namespace: ns, Name: name})
	if err != nil {
		return false
	}
	return repo.IsMirror()
}

// labRepo loads the repo-scoped handle and current principal, enforcing read
// visibility. Private repositories are only readable by namespace members;
// public repositories are readable by anyone. Write-protected callers should
// rely on labRequireWrite in addition to a successful labRepo load.
func (s *server) labRepo(w http.ResponseWriter, r *http.Request) (*store.Repo, *store.User, bool) {
	ns := r.PathValue("namespace")
	name := r.PathValue("repo")
	if ns == "" {
		ns = r.PathValue("ns")
	}
	if name == "" {
		name = r.PathValue("name")
	}
	if ns == "" || name == "" {
		labErr(w, http.StatusNotFound, fmt.Errorf("repo not found"))
		return nil, nil, false
	}
	rr := store.RepoRef{Namespace: ns, Name: name}
	if _, err := s.cs.OpenRepo(rr); err != nil {
		labErr(w, http.StatusNotFound, err)
		return nil, nil, false
	}
	u, _ := s.labPrincipal(r)
	var userID *int64
	if u != nil {
		userID = &u.ID
	}
	// Enforce reading a private repo requires membership.
	if !s.cs.UserCanReadRepo(rr, userID) {
		labErr(w, http.StatusNotFound, fmt.Errorf("repo not found"))
		return nil, nil, false
	}
	repo, err := s.cs.OpenRepo(rr)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return nil, nil, false
	}
	return repo, u, true
}

// ---- users ----

func (s *server) labCurrentUser(w http.ResponseWriter, r *http.Request) {
	u, _ := s.labPrincipal(r)
	if u == nil {
		labErr(w, http.StatusUnauthorized, fmt.Errorf("unauthorized"))
		return
	}
	labJSON(w, http.StatusOK, u)
}

func (s *server) labListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.cs.ListUsers()
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	labJSON(w, http.StatusOK, users)
}

type labUserReq struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name,omitempty"`
}

func (s *server) labCreateUser(w http.ResponseWriter, r *http.Request) {
	if _, level := s.labPrincipal(r); level != "admin" && level != "write" {
		labErr(w, http.StatusForbidden, fmt.Errorf("forbidden"))
		return
	}
	var req labUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		labErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Username == "" {
		labErr(w, http.StatusBadRequest, fmt.Errorf("username required"))
		return
	}
	u, err := s.cs.CreateUser(req.Username, req.DisplayName)
	if err != nil {
		labErr(w, http.StatusConflict, err)
		return
	}
	labJSON(w, http.StatusOK, u)
}

func (s *server) labGetUser(w http.ResponseWriter, r *http.Request) {
	u, err := s.cs.GetUserByUsername(r.PathValue("username"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	labJSON(w, http.StatusOK, u)
}

type labTokenReq struct {
	Token string `json:"token"`
	Level string `json:"level,omitempty"`
}

func (s *server) labCreateToken(w http.ResponseWriter, r *http.Request) {
	u, err := s.cs.GetUserByUsername(r.PathValue("username"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	var req labTokenReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		labErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Token == "" {
		labErr(w, http.StatusBadRequest, fmt.Errorf("token required"))
		return
	}
	level := req.Level
	if level == "" {
		level = "write"
	}
	tok, err := s.cs.CreateToken(req.Token, u.ID, level)
	if err != nil {
		labErr(w, http.StatusConflict, err)
		return
	}
	labJSON(w, http.StatusOK, tok)
}

func (s *server) labListTokens(w http.ResponseWriter, r *http.Request) {
	u, err := s.cs.GetUserByUsername(r.PathValue("username"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	tokens, err := s.cs.ListTokens(u.ID)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	// Redact the raw token text in the listing.
	type tokView struct {
		ID      int64  `json:"id"`
		UserID  int64  `json:"user_id"`
		Level   string `json:"level"`
		Created int64  `json:"created_ms"`
	}
	var out []tokView
	for _, t := range tokens {
		out = append(out, tokView{ID: t.ID, UserID: t.UserID, Level: t.Level, Created: t.Created.UnixMilli()})
	}
	labJSON(w, http.StatusOK, out)
}

func (s *server) labDeleteToken(w http.ResponseWriter, r *http.Request) {
	if err := s.cs.DeleteToken(r.PathValue("token")); err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	labJSON(w, http.StatusOK, labResponse{"deleted": true})
}

// ---- namespace members ----

type labMemberReq struct {
	Username string `json:"username"`
	Role     string `json:"role,omitempty"`
}

func (s *server) labAddMember(w http.ResponseWriter, r *http.Request) {
	if _, level := s.labPrincipal(r); level != "admin" && level != "write" {
		labErr(w, http.StatusForbidden, fmt.Errorf("forbidden"))
		return
	}
	var req labMemberReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		labErr(w, http.StatusBadRequest, err)
		return
	}
	u, err := s.cs.GetUserByUsername(req.Username)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	role := req.Role
	if role == "" {
		role = "member"
	}
	if err := s.cs.AddNamespaceMember(r.PathValue("namespace"), u.ID, role); err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	labJSON(w, http.StatusOK, labResponse{"namespace": r.PathValue("namespace"), "username": req.Username, "role": role})
}

func (s *server) labListMembers(w http.ResponseWriter, r *http.Request) {
	members, err := s.cs.ListNamespaceMembers(r.PathValue("namespace"))
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	var out []labResponse
	for _, m := range members {
		u, _ := s.cs.GetUser(m.UserID)
		entry := labResponse{"user_id": m.UserID, "role": m.Role}
		if u != nil {
			entry["username"] = u.Username
		}
		out = append(out, entry)
	}
	labJSON(w, http.StatusOK, out)
}

func (s *server) labRemoveMember(w http.ResponseWriter, r *http.Request) {
	if _, level := s.labPrincipal(r); level != "admin" && level != "write" {
		labErr(w, http.StatusForbidden, fmt.Errorf("forbidden"))
		return
	}
	u, err := s.cs.GetUserByUsername(r.PathValue("username"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	if err := s.cs.RemoveNamespaceMember(r.PathValue("namespace"), u.ID); err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	labJSON(w, http.StatusOK, labResponse{"removed": r.PathValue("username")})
}

// ---- repositories ----

// labRepoMeta describes the hosting metadata returned by repo endpoints.
type labRepoMeta struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	Visibility     string `json:"visibility"`
	DefaultBranch  string `json:"default_branch"`
	Kind           string `json:"kind"`
	MirrorURL      string `json:"mirror_url,omitempty"`
	MirrorBranch   string `json:"mirror_branch,omitempty"`
	MirrorInterval int    `json:"mirror_interval,omitempty"`
	MirrorLastRev  string `json:"mirror_last_rev,omitempty"`
	MirrorLastSync int64  `json:"mirror_last_sync_ms,omitempty"`
	MirrorLastErr  string `json:"mirror_last_error,omitempty"`
	Created        int64  `json:"created_ms"`
}

func (s *server) labListRepos(w http.ResponseWriter, r *http.Request) {
	u, _ := s.labPrincipal(r)
	var userID *int64
	if u != nil {
		userID = &u.ID
	}
	repos, err := s.cs.List()
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	var out []labRepoMeta
	for _, rr := range repos {
		if !s.cs.UserCanReadRepo(rr, userID) {
			continue
		}
		rp, _ := s.cs.OpenRepo(rr)
		if rp == nil {
			continue
		}
		meta, err := rp.RepoMeta()
		if err != nil {
			continue
		}
		out = append(out, labRepoMeta{
			Namespace: rr.Namespace, Name: rr.Name,
			Description: meta.Description, Visibility: meta.Visibility,
			DefaultBranch: meta.DefaultBranch, Created: 0,
			Kind:           meta.Kind,
			MirrorURL:      meta.MirrorURL,
			MirrorBranch:   meta.MirrorBranch,
			MirrorInterval: meta.MirrorInterval,
			MirrorLastRev:  meta.MirrorLastRev,
			MirrorLastSync: meta.MirrorLastSync,
			MirrorLastErr:  meta.MirrorLastErr,
		})
	}
	labJSON(w, http.StatusOK, out)
}

type labCreateRepoReq struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	Description    string `json:"description,omitempty"`
	Visibility     string `json:"visibility,omitempty"`
	DefaultBranch  string `json:"default_branch,omitempty"`
	Kind           string `json:"kind,omitempty"`
	MirrorURL      string `json:"mirror_url,omitempty"`
	MirrorBranch   string `json:"mirror_branch,omitempty"`
	MirrorInterval int    `json:"mirror_interval_secs,omitempty"`
	MirrorToken    string `json:"mirror_token,omitempty"`
}

func (s *server) labCreateRepo(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		var req labCreateRepoReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		if req.Namespace == "" || req.Name == "" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("namespace and name required"))
			return
		}
		if req.Kind == "mirror" && req.MirrorURL == "" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("mirror_url required for mirror repository"))
			return
		}
		_, err := s.cs.Create(store.RepoRef{Namespace: req.Namespace, Name: req.Name})
		if err != nil {
			labErr(w, http.StatusConflict, err)
			return
		}
		repo, err := s.cs.OpenRepo(store.RepoRef{Namespace: req.Namespace, Name: req.Name})
		if err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}
		vis := defaultStr(req.Visibility, "public")
		def := defaultStr(req.DefaultBranch, "main")
		if err := repo.UpdateRepoMeta(store.RepoMeta{Description: req.Description, Visibility: vis, DefaultBranch: def}); err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}
		if req.Kind == "mirror" {
			interval := req.MirrorInterval
			if interval <= 0 {
				interval = 300
			}
			if err := repo.UpdateMirrorMeta(store.RepoMeta{
				Kind: "mirror", MirrorURL: req.MirrorURL,
				MirrorBranch: defaultStr(req.MirrorBranch, def), MirrorInterval: interval,
				MirrorToken: req.MirrorToken,
			}); err != nil {
				labErr(w, http.StatusInternalServerError, err)
				return
			}
		}
		labJSON(w, http.StatusOK, labRepoMeta{
			Namespace: req.Namespace, Name: req.Name,
			Description: req.Description, Visibility: vis, DefaultBranch: def, Created: 1,
			Kind:           defaultStr(req.Kind, "normal"),
			MirrorURL:      req.MirrorURL,
			MirrorBranch:   defaultStr(req.MirrorBranch, def),
			MirrorInterval: req.MirrorInterval,
		})
	})(w, r)
}

func (s *server) labGetRepo(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	meta, err := repo.RepoMeta()
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	labJSON(w, http.StatusOK, labRepoMeta{
		Namespace: repo.Namespace, Name: repo.Name,
		Description: meta.Description, Visibility: meta.Visibility, DefaultBranch: meta.DefaultBranch,
		Kind:           meta.Kind,
		MirrorURL:      meta.MirrorURL,
		MirrorBranch:   meta.MirrorBranch,
		MirrorInterval: meta.MirrorInterval,
		MirrorLastRev:  meta.MirrorLastRev,
		MirrorLastSync: meta.MirrorLastSync,
		MirrorLastErr:  meta.MirrorLastErr,
	})
}

type labUpdateRepoReq struct {
	Description   string `json:"description,omitempty"`
	Visibility    string `json:"visibility,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

func (s *server) labUpdateRepo(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		var req labUpdateRepoReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		meta, _ := repo.RepoMeta()
		if req.Description != "" {
			meta.Description = req.Description
		}
		if req.Visibility != "" {
			meta.Visibility = req.Visibility
		}
		if req.DefaultBranch != "" {
			meta.DefaultBranch = req.DefaultBranch
		}
		if err := repo.UpdateRepoMeta(meta); err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}
		labJSON(w, http.StatusOK, labRepoMeta{
			Namespace: repo.Namespace, Name: repo.Name,
			Description: meta.Description, Visibility: meta.Visibility, DefaultBranch: meta.DefaultBranch,
		})
	})(w, r)
}

func (s *server) labDeleteRepo(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		if err := s.cs.Delete(store.RepoRef{Namespace: r.PathValue("namespace"), Name: r.PathValue("repo")}); err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{"deleted": true})
	})(w, r)
}

// ---- revisions ----

// ---- helper utilities ----

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func idsToStrSlice(ids []object.ID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func parseIID(r *http.Request) (int64, error) {
	var out int64
	if _, err := fmt.Sscan(r.PathValue("iid"), &out); err != nil {
		return 0, fmt.Errorf("invalid iid")
	}
	return out, nil
}

// parseAssetID parses an asset id path segment.
func parseAssetID(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// intAtoi parses a non-negative integer; invalid/empty returns 0.
func intAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// treeOf resolves a ref expression to a root tree id. When ref is empty it
// defaults to the newest revision's snapshot tree. It supports revision ids,
// snapshot hashes, branch/tag names, and "@".
func treeOf(ws *revision.Workspace, repo *store.Repo, ref string) (object.ID, error) {
	if ref == "" {
		revs, err := ws.Log()
		if err != nil || len(revs) == 0 {
			return object.ID{}, fmt.Errorf("no revisions")
		}
		snap, err := repo.GetSnapshot(revs[0].Hash)
		if err != nil {
			return object.ID{}, err
		}
		return snap.TreeID, nil
	}
	id, err := resolveRefAny(ws, repo, ref)
	if err != nil {
		return object.ID{}, err
	}
	snap, err := repo.GetSnapshot(id)
	if err != nil {
		return object.ID{}, err
	}
	return snap.TreeID, nil
}

// resolveSnapshotOf resolves a ref to its snapshot, tolerating a raw tree id.
func resolveSnapshotOf(ws *revision.Workspace, repo *store.Repo, ref string) (*store.Snapshot, error) {
	if id, err := object.HexToID(ref); err == nil {
		if snap, err := repo.GetSnapshot(id); err == nil {
			return snap, nil
		}
	}
	id, err := resolveRefAny(ws, repo, ref)
	if err != nil {
		return nil, err
	}
	return repo.GetSnapshot(id)
}

// resolveRefAny resolves a revision identifier: a revision id, a revision id
// prefix, a snapshot hash, a branch/tag name, or "@". It returns the current
// snapshot hash of the matched revision.
func resolveRefAny(ws *revision.Workspace, repo *store.Repo, ref string) (object.ID, error) {
	if ref == "" || ref == "@" {
		revs, err := ws.Log()
		if err != nil || len(revs) == 0 {
			return object.ID{}, fmt.Errorf("no revisions")
		}
		return revs[0].Hash, nil
	}
	if ref == "@-" {
		revs, err := ws.Log()
		if err != nil || len(revs) == 0 {
			return object.ID{}, fmt.Errorf("no revisions")
		}
		snap, err := repo.GetSnapshot(revs[0].Hash)
		if err != nil {
			return object.ID{}, err
		}
		if len(snap.Parents) == 0 {
			return object.ID{}, fmt.Errorf("no parent")
		}
		return snap.Parents[0], nil
	}
	// Try a ref (branch/tag).
	if rf, err := ws.GetRef(ref); err == nil && rf != nil {
		return resolveRefAny(ws, repo, rf.Target)
	}
	// Try a snapshot hash directly.
	if id, err := object.HexToID(ref); err == nil {
		if _, err := repo.GetSnapshot(id); err == nil {
			return id, nil
		}
	}
	// Revisions: exact id or prefix.
	revs, err := ws.Log()
	if err != nil {
		return object.ID{}, err
	}
	for _, rv := range revs {
		if rv.ID == ref {
			return rv.Hash, nil
		}
	}
	for _, rv := range revs {
		if strings.HasPrefix(rv.ID, ref) {
			return rv.Hash, nil
		}
	}
	return object.ID{}, fmt.Errorf("unknown ref %q", ref)
}

// resolveRevID resolves a revision identifier (revision id, id prefix,
// branch/tag name, "@", or a snapshot hash) down to the owning revision id
// string. Empty ref resolves to the newest revision.
func resolveRevID(ws *revision.Workspace, repo *store.Repo, ref string) (string, error) {
	hash, err := resolveRefAny(ws, repo, ref)
	if err != nil {
		return "", err
	}
	revs, err := ws.Log()
	if err != nil {
		return "", err
	}
	for _, rv := range revs {
		if rv.Hash == hash {
			return rv.ID, nil
		}
	}
	// Ref pointed at a raw snapshot hash not owned by a listed revision (rare);
	// best effort: return the hash as a revision id lookup will fail.
	return hash.String(), nil
}

func (s *server) labListRevisions(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	ref := r.URL.Query().Get("ref")
	start := ""
	if ref != "" {
		id, err := resolveRevID(ws, repo, ref)
		if err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		start = id
	}
	revs, err := ws.Log()
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	var out []labResponse
	for _, cr := range revs {
		snap, err := repo.GetSnapshot(cr.Hash)
		if err != nil {
			continue
		}
		out = append(out, labResponse{
			"revision_id": cr.ID,
			"snapshot":    snap.RevisionHash.String(),
			"tree":        snap.TreeID.String(),
			"description": snap.Description,
			"author":      snap.Author.String(),
			"created_ms":  snap.CommitTime.UnixMilli(),
		})
	}
	_ = start
	labJSON(w, http.StatusOK, out)
}

func (s *server) labRevisionFiles(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	id, err := resolveRevID(ws, repo, r.PathValue("rev"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	changes, err := ws.FilesChanged(id)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	out := make([]labResponse, 0, len(changes))
	for _, c := range changes {
		out = append(out, labResponse{"path": c.Path, "status": c.Status})
	}
	labJSON(w, http.StatusOK, out)
}

// labGraph returns the revision DAG as a list of nodes (parents before
// children), suitable for a graph view.
func (s *server) labGraph(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	revs, err := ws.Log()
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	limit := intAtoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	type node struct {
		id   string
		snap *store.Snapshot
		rev  *store.Revision
	}
	byID := map[string]*node{}
	snapOwner := map[string]string{}
	for _, re := range revs {
		snap, err := repo.GetSnapshot(re.Hash)
		if err != nil {
			continue
		}
		byID[re.ID] = &node{id: re.ID, snap: snap, rev: re}
		snapOwner[snap.RevisionHash.String()] = re.ID
	}
	// Topological order: parents before children (chronological by commit time).
	var order []*node
	visited := map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		n := byID[id]
		if n == nil || visited[id] {
			return
		}
		visited[id] = true
		for _, p := range n.snap.Parents {
			if owner, ok := snapOwner[p.String()]; ok {
				visit(owner)
			}
		}
		order = append(order, n)
	}
	var starts []string
	for _, re := range revs {
		starts = append(starts, re.ID)
	}
	sort.SliceStable(starts, func(i, j int) bool {
		return byID[starts[i]].rev.Created.After(byID[starts[j]].rev.Created)
	})
	for _, id := range starts {
		visit(id)
	}

	// Determine heads (nodes that are not anyone's parent).
	heads := map[string]bool{}
	for _, n := range byID {
		heads[n.id] = true
	}
	for _, n := range byID {
		for _, p := range n.snap.Parents {
			if owner, ok := snapOwner[p.String()]; ok {
				delete(heads, owner)
			}
		}
	}

	out := make([]labResponse, 0, len(order))
	for _, n := range order {
		parents := make([]string, 0, len(n.snap.Parents))
		for _, p := range n.snap.Parents {
			if owner, ok := snapOwner[p.String()]; ok {
				parents = append(parents, owner)
			} else {
				parents = append(parents, p.String())
			}
		}
		out = append(out, labResponse{
			"revision_id": n.id,
			"snapshot":    n.rev.Hash.String(),
			"message":     n.snap.Description,
			"author":      n.snap.Author.String(),
			"parents":     parents,
			"is_head":     heads[n.id],
			"created_ms":  n.snap.CommitTime.UnixMilli(),
		})
	}
	if len(out) > limit {
		out = out[:limit]
	}
	labJSON(w, http.StatusOK, labResponse{"graph": out})
}

func (s *server) labGetRevision(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	id, err := resolveRevID(ws, repo, r.PathValue("rev"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	cr, err := repo.GetRevision(id)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	snap, err := repo.GetSnapshot(cr.Hash)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	labJSON(w, http.StatusOK, labResponse{
		"revision_id":   cr.ID,
		"snapshot":      snap.RevisionHash.String(),
		"tree":          snap.TreeID.String(),
		"parents":       idsToStrSlice(snap.Parents),
		"description":   snap.Description,
		"author":        snap.Author.String(),
		"created_ms":    snap.CommitTime.UnixMilli(),
		"changed_paths": cr.ChangedPaths,
	})
}

func (s *server) labRevisionDiff(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	id, err := resolveRevID(ws, repo, r.PathValue("rev"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	cr, err := repo.GetRevision(id)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	snap, err := repo.GetSnapshot(cr.Hash)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	// Single-parent/linear model: the diff base is always the first parent.
	var parent object.ID
	if len(snap.Parents) > 0 {
		parent = snap.Parents[0]
	}
	var diffs []revision.FileDiff
	if parent.IsZero() {
		// Root revision: compare against empty tree (added files).
		diffs, err = ws.DiffContent(object.ID{}, snap.TreeID)
	} else {
		pSnap, perr := repo.GetSnapshot(parent)
		if perr != nil {
			labErr(w, http.StatusInternalServerError, perr)
			return
		}
		diffs, err = ws.DiffContent(pSnap.TreeID, snap.TreeID)
	}
	if err != nil {
		labErr(w, http.StatusBadRequest, err)
		return
	}
	labJSON(w, http.StatusOK, diffs)
}

// labWriteFiles is the unified write entry: it applies a batch of file changes
// (single or multi) and lands them on a branch. It default-amends onto the
// branch's current revision (revision_id stays stable) unless new_commit is
// true, in which case a fresh revision is created. Every write must resolve to
// a branch (ref, or the repo's default branch).
func (s *server) labWriteFiles(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		req, err := decodeJSONBody[commitReq](r)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		if len(req.Changes) == 0 {
			labErr(w, http.StatusBadRequest, fmt.Errorf("at least one change required"))
			return
		}
		author := store.Author{Name: "easyvcs", Email: "easyvcs@example.com"}
		if req.Author != nil {
			author = *req.Author
		}
		ws := revision.NewWorkspace(repo)

		// Resolve the target branch (ref > default branch).
		ref := req.Ref
		isDefault := false
		if ref == "" {
			meta, err := repo.RepoMeta()
			if err != nil {
				labErr(w, http.StatusInternalServerError, err)
				return
			}
			ref = meta.DefaultBranch
			isDefault = true
		}
		branch, berr := ws.GetRef(ref)
		if berr != nil || branch == nil || branch.Kind != store.RefBranch {
			// A missing branch is only tolerated when it is the repo's default
			// (the natural landing point for a first write); the write then
			// creates it lazily at the resulting revision. An explicit ref to a
			// non-existent branch is an error.
			if !isDefault {
				labErr(w, http.StatusBadRequest, fmt.Errorf("branch %q not found; every write must target a branch", ref))
				return
			}
			branch = &store.Ref{Name: ref, Kind: store.RefBranch}
		}

		var revisionIDOverride string
		var parentID object.ID

		switch {
		case req.RevisionID != "":
			// Explicit amend to a given revision.
			revisionIDOverride = req.RevisionID
			if req.ParentHash != "" {
				if parentID, err = object.HexToID(req.ParentHash); err != nil {
					labErr(w, http.StatusBadRequest, err)
					return
				}
			}
		case req.NewCommit:
			// Fresh revision (parent = branch tip or explicit parent_hash).
			if req.ParentHash != "" {
				if parentID, err = object.HexToID(req.ParentHash); err != nil {
					labErr(w, http.StatusBadRequest, err)
					return
				}
			} else if branch.Target != "" {
				parentID, err = resolveRefAny(ws, repo, branch.Name)
				if err != nil {
					labErr(w, http.StatusBadRequest, err)
					return
				}
			}
			// else: no branch tip yet (first commit) -> root, parent stays zero.
		default:
			// Amend onto the branch tip (revision_id stable). If the default
			// branch has no tip yet, this is a fresh root commit instead.
			revisionIDOverride = branch.Target
			parentID = object.ID{}
		}

		var changes []revision.FileChangeSpec
		for _, c := range req.Changes {
			changes = append(changes, revision.FileChangeSpec{Path: c.Path, Content: []byte(c.Content), Delete: c.Delete})
		}
		snap, ch, err := ws.CommitFromChanges(parentID, changes, req.Description, author, revisionIDOverride)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		// Re-point the branch to the resulting revision (new or amended).
		if _, err := ws.SetRef(branch.Name, store.RefBranch, ch.ID); err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}

		changed := make([]string, 0, len(req.Changes))
		for _, c := range req.Changes {
			changed = append(changed, c.Path)
		}
		labJSON(w, http.StatusOK, labResponse{
			"revision_id": ch.ID,
			"snapshot":    snap.RevisionHash.String(),
			"tree":        snap.TreeID.String(),
			"amended":     revisionIDOverride != "",
			"changed":     changed,
		})
	})(w, r)
}

func (s *server) labRebase(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		ws := revision.NewWorkspace(repo)
		id, err := resolveRevID(ws, repo, r.PathValue("rev"))
		if err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		req, err := decodeJSONBody[rebaseReq](r)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		var parents []object.ID
		for _, p := range req.NewParents {
			pid, err := object.HexToID(p)
			if err != nil {
				labErr(w, http.StatusBadRequest, err)
				return
			}
			parents = append(parents, pid)
		}
		snap, ch, err := ws.Rebase(id, parents)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{"revision_id": ch.ID, "snapshot": snap.RevisionHash.String()})
	})(w, r)
}

// labRebaseMany rebases a sequence of revisions onto a new base in one chained
// operation (e.g. moving [D, H] onto F to drop G).
type labRebaseManyReq struct {
	Revisions []string `json:"revisions"`
	Onto      string   `json:"onto"`
}

func (s *server) labRebaseMany(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		ws := revision.NewWorkspace(repo)
		var req labRebaseManyReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		if len(req.Revisions) == 0 || req.Onto == "" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("revisions and onto required"))
			return
		}
		// Resolve onto to a snapshot hash (object id).
		ontoID, err := resolveRefAny(ws, repo, req.Onto)
		if err != nil {
			labErr(w, http.StatusBadRequest, fmt.Errorf("onto %s: %w", req.Onto, err))
			return
		}
		// Resolve each revision id.
		var revIDs []string
		for _, rid := range req.Revisions {
			resolved, err := resolveRevID(ws, repo, rid)
			if err != nil {
				labErr(w, http.StatusBadRequest, fmt.Errorf("revision %s: %w", rid, err))
				return
			}
			revIDs = append(revIDs, resolved)
		}
		moved, err := ws.RebaseMany(revIDs, ontoID)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		out := make([]labResponse, 0, len(moved))
		for _, m := range moved {
			snap, _ := repo.GetSnapshot(m.Hash)
			hash := ""
			if snap != nil {
				hash = snap.RevisionHash.String()
			}
			out = append(out, labResponse{"revision_id": m.ID, "snapshot": hash})
		}
		labJSON(w, http.StatusOK, labResponse{"moved": out})
	})(w, r)
}

// labDrop erases a revision from history by rebasing its descendants onto the
// revision's parent (no op-log preserved).
func (s *server) labDrop(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		ws := revision.NewWorkspace(repo)
		id, err := resolveRevID(ws, repo, r.PathValue("rev"))
		if err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		moved, err := ws.Drop(id)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		out := make([]labResponse, 0, len(moved))
		for _, m := range moved {
			snap, _ := repo.GetSnapshot(m.Hash)
			hash := ""
			if snap != nil {
				hash = snap.RevisionHash.String()
			}
			out = append(out, labResponse{"revision_id": m.ID, "snapshot": hash})
		}
		labJSON(w, http.StatusOK, labResponse{"dropped": id, "moved": out})
	})(w, r)
}

// labRevert creates a new revision that undoes a revision's changes, preserving
// history (inverse-patch model). Optionally onto a specific ref.
func (s *server) labRevert(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		ws := revision.NewWorkspace(repo)
		id, err := resolveRevID(ws, repo, r.PathValue("rev"))
		if err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		var onto object.ID
		if ontoStr := r.URL.Query().Get("onto"); ontoStr != "" {
			ontoID, err := resolveRefAny(ws, repo, ontoStr)
			if err != nil {
				labErr(w, http.StatusBadRequest, fmt.Errorf("onto %s: %w", ontoStr, err))
				return
			}
			onto = ontoID
		}
		snap, ch, err := ws.Revert(id, onto)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{
			"revision_id": ch.ID,
			"snapshot":    snap.RevisionHash.String(),
			"tree":        snap.TreeID.String(),
		})
	})(w, r)
}

func (s *server) labSquash(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		ws := revision.NewWorkspace(repo)
		id, err := resolveRevID(ws, repo, r.PathValue("rev"))
		if err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		snap, ch, err := ws.Squash(id)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{"revision_id": ch.ID, "snapshot": snap.RevisionHash.String()})
	})(w, r)
}

type labResolveReq struct {
	Path string `json:"path"`
	Side int    `json:"side"`
}

func (s *server) labResolve(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		ws := revision.NewWorkspace(repo)
		id, err := resolveRevID(ws, repo, r.PathValue("rev"))
		if err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		var req labResolveReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		snap, ch, err := ws.Resolve(id, req.Path, req.Side)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{"revision_id": ch.ID, "snapshot": snap.RevisionHash.String()})
	})(w, r)
}

// ---- content & search ----

func (s *server) labCompare(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	from := r.URL.Query().Get("from")
	to := r.URL.Query().Get("to")
	aTree, err := treeOf(ws, repo, from)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	bTree, err := treeOf(ws, repo, to)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	diffs, err := ws.DiffContent(aTree, bTree)
	if err != nil {
		labErr(w, http.StatusBadRequest, err)
		return
	}
	labJSON(w, http.StatusOK, diffs)
}

func (s *server) labTree(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	treeID, err := treeOf(ws, repo, r.URL.Query().Get("ref"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	tree, err := ws.ReadTree(treeID)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	path := r.URL.Query().Get("path")
	var entries []labResponse
	for _, e := range tree.SortedEntries() {
		entries = append(entries, labResponse{
			"name": e.Name, "kind": strings.ToLower(e.Kind.String()), "id": e.ID.String(),
		})
	}
	labJSON(w, http.StatusOK, labResponse{"tree": treeID.String(), "path": path, "entries": entries})
}

func (s *server) labBlob(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	treeID, err := treeOf(ws, repo, r.URL.Query().Get("ref"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	tree, err := ws.ReadTree(treeID)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	entry, err := ws.FindEntry(tree, r.URL.Query().Get("path"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	data, err := ws.ReadBlob(entry.ID)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// labContents reads a single file (Gitea-style base64) at a ref.
func (s *server) labContents(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	treeID, err := treeOf(ws, repo, r.URL.Query().Get("ref"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	tree, err := ws.ReadTree(treeID)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	path := strings.TrimPrefix(r.PathValue("path"), "/")
	entry, err := ws.FindEntry(tree, path)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	if entry.Kind != object.KindBlob {
		labErr(w, http.StatusForbidden, fmt.Errorf("not a file: %s", path))
		return
	}
	data, err := ws.ReadBlob(entry.ID)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	labJSON(w, http.StatusOK, labResponse{
		"path":    path,
		"content": base64.StdEncoding.EncodeToString(data),
		"sha":     entry.ID.String(),
		"size":    len(data),
	})
}

// labBlameLine is retained for backwards compatibility of the JSON shape; the
// labBlame handler now returns []revision.AnnotationLine directly.
type labBlameLine struct {
	LineNumber int    `json:"line_number"`
	RevisionID string `json:"revision_id"`
	Author     string `json:"author"`
	Message    string `json:"message"`
	Content    string `json:"content"`
}

func (s *server) labBlame(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	path := r.URL.Query().Get("path")
	if path == "" {
		labErr(w, http.StatusBadRequest, fmt.Errorf("path required"))
		return
	}
	ref := r.URL.Query().Get("ref")
	startID := ""
	if ref != "" {
		id, err := resolveRevID(ws, repo, ref)
		if err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		startID = id
	}
	annotations, err := ws.Annotate(startID, path)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	labJSON(w, http.StatusOK, annotations)
}

func (s *server) labSearch(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	q := r.URL.Query().Get("q")
	if q == "" {
		labErr(w, http.StatusBadRequest, fmt.Errorf("q required"))
		return
	}
	treeID, err := treeOf(ws, repo, r.URL.Query().Get("ref"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	paths, err := ws.CollectPaths(treeID)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	tree := ws.MustTree(treeID)
	var hits []labResponse
	for _, p := range paths {
		entry, err := ws.FindEntry(tree, p)
		if err != nil {
			continue
		}
		data, err := ws.ReadBlob(entry.ID)
		if err != nil {
			continue
		}
		if strings.Contains(string(data), q) {
			hits = append(hits, labResponse{"path": p})
		}
	}
	labJSON(w, http.StatusOK, hitResults{Matches: hits})
}

type hitResults struct {
	Matches []labResponse `json:"matches"`
}

// labArchive streams a tarball of the repository at a given ref.
func (s *server) labArchive(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	treeID, err := treeOf(ws, repo, r.PathValue("ref"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	tmp, err := os.MkdirTemp("", "easyvcs-archive-*")
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	defer os.RemoveAll(tmp)
	if err := ws.Materialize(treeID, tmp); err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename=\""+repo.Name+"-"+r.PathValue("ref")+".tar.gz\"")
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	err = filepath.Walk(tmp, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(tmp, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		if info.IsDir() {
			hdr, hErr := tar.FileInfoHeader(info, "")
			if hErr != nil {
				return hErr
			}
			hdr.Name = filepath.ToSlash(rel) + "/"
			return tw.WriteHeader(hdr)
		}
		hdr, hErr := tar.FileInfoHeader(info, "")
		if hErr != nil {
			return hErr
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, fErr := os.Open(path)
		if fErr != nil {
			return fErr
		}
		defer f.Close()
		_, cErr := io.Copy(tw, f)
		return cErr
	})
	if err != nil {
		// Header already sent; nothing else to do.
		return
	}
	if err := tw.Close(); err != nil {
		return
	}
	if err := gz.Close(); err != nil {
		return
	}
}

func (s *server) labConflicts(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	treeID, err := treeOf(ws, repo, r.URL.Query().Get("ref"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	atoms, err := ws.ConflictsInTree(treeID)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	labJSON(w, http.StatusOK, atoms)
}

func (s *server) labHistory(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	path := r.URL.Query().Get("path")
	if path == "" {
		labErr(w, http.StatusBadRequest, fmt.Errorf("path required"))
		return
	}
	opt := revision.HistoryOpt{
		Start: r.URL.Query().Get("ref"),
		Desc:  strings.EqualFold(r.URL.Query().Get("order"), "newest") || r.URL.Query().Get("order") == "",
		Limit: intAtoi(r.URL.Query().Get("limit")),
	}
	edits, err := ws.FileHistoryOpt(opt, path)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	labJSON(w, http.StatusOK, edits)
}

func (s *server) labStatus(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	ws := revision.NewWorkspace(repo)
	treeID, err := treeOf(ws, repo, r.URL.Query().Get("ref"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	// No working copy in this model: report the revision that owns this tree,
	// plus its changed paths relative to its parent.
	revs, err := ws.Log()
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	for _, cr := range revs {
		if cr.Hash.String() == treeID.String() {
			labJSON(w, http.StatusOK, labResponse{"revision_id": cr.ID, "changes": cr.ChangedPaths})
			return
		}
	}
	labJSON(w, http.StatusOK, labResponse{"changes": nil})
}

// ---- refs ----

func (s *server) labListBranches(w http.ResponseWriter, r *http.Request) {
	s.labListRefs(w, r, store.RefBranch)
}

func (s *server) labListTags(w http.ResponseWriter, r *http.Request) {
	s.labListRefs(w, r, store.RefTag)
}

func (s *server) labListRefs(w http.ResponseWriter, r *http.Request, kind store.RefKind) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	refs, err := revision.NewWorkspace(repo).ListRefs()
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	var out []labResponse
	for _, rf := range refs {
		if rf.Kind != kind {
			continue
		}
		out = append(out, labResponse{"name": rf.Name, "revision_id": rf.Target})
	}
	labJSON(w, http.StatusOK, out)
}

type labSetRefReq struct {
	Name       string `json:"name,omitempty"`
	RevisionID string `json:"revision_id"`
}

func (s *server) labSetBranch(w http.ResponseWriter, r *http.Request) {
	s.labSetRef(w, r, store.RefBranch)
}

func (s *server) labSetTag(w http.ResponseWriter, r *http.Request) {
	s.labSetRef(w, r, store.RefTag)
}

func (s *server) labSetRef(w http.ResponseWriter, r *http.Request, kind store.RefKind) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		var req labSetRefReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		name := req.Name
		if p := r.PathValue("name"); p != "" {
			name = p
		}
		if name == "" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("name required"))
			return
		}
		target := req.RevisionID
		if target == "" || target == "@" {
			// Resolve "@" (or empty) to the current revision id.
			id, err := resolveRevID(revision.NewWorkspace(repo), repo, "@")
			if err != nil {
				labErr(w, http.StatusBadRequest, err)
				return
			}
			target = id
		}
		ref, err := revision.NewWorkspace(repo).SetRef(name, kind, target)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{"name": ref.Name, "kind": ref.Kind, "revision_id": ref.Target})
	})(w, r)
}

func (s *server) labDeleteBranch(w http.ResponseWriter, r *http.Request) {
	s.labDeleteRef(w, r)
}

func (s *server) labDeleteTag(w http.ResponseWriter, r *http.Request) {
	s.labDeleteRef(w, r)
}

func (s *server) labDeleteRef(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		name := r.PathValue("name")
		if err := revision.NewWorkspace(repo).DeleteRef(name); err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{"deleted": name})
	})(w, r)
}

// ---- fork ----

type labForkReq struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (s *server) labFork(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		src, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		var req labForkReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		if req.Name == "" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("name required"))
			return
		}
		dstNS := req.Namespace
		if dstNS == "" {
			dstNS = src.Namespace
		}
		dst, err := s.cs.Fork(src.RepoRef(), store.RepoRef{Namespace: dstNS, Name: req.Name})
		if err != nil {
			labErr(w, http.StatusConflict, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{
			"forked_from": src.String(),
			"namespace":   dst.Namespace,
			"name":        dst.Name,
		})
	})(w, r)
}

// ---- releases (backed by the artifactkit generic package registry) ----
//
// A Lab release maps to an artifact in the "generic" format:
//   repository = "ns:repo"   (colon, so the generic URL path stays flat)
//   version    = the release tag
//   blobs      = the release assets (filename -> immutable blob)
//
// The repository uses "ns:repo" rather than "ns/repo" because the generic
// protocol addresses artifacts as /pkgs/generic/{name}/{version}/{filename}
// and a slash in the name would nest an extra path segment.
//
// The Lab /releases endpoints are a convenience view over that substrate, so
// clients can also access raw artifacts directly at /pkgs/generic/ns:repo/tag/<file>.

func releaseRepository(repo *store.Repo) string { return repo.Namespace + ":" + repo.Name }

type labReleaseView struct {
	Tag         string    `json:"tag"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	RevisionID  string    `json:"revision_id"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	Created     time.Time `json:"created"`
	Assets      []string  `json:"assets"`
}

type labCreateReleaseReq struct {
	Tag         string `json:"tag"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	RevisionID  string `json:"revision_id,omitempty"`
	Draft       bool   `json:"draft,omitempty"`
	Prerelease  bool   `json:"prerelease,omitempty"`
}

// releaseFromArtifact renders a artifactkit Artifact into a Lab release view.
func releaseFromArtifact(a artifactkit.Artifact) labReleaseView {
	view := labReleaseView{Tag: a.Version}
	// Description/name/revision are round-tripped through the artifact's
	// proprietary bytes as JSON.
	var meta labReleaseMeta
	if len(a.Proprietary) > 0 {
		_ = json.Unmarshal(a.Proprietary, &meta)
	}
	view.Name = meta.Name
	view.Description = meta.Description
	view.RevisionID = meta.RevisionID
	view.Draft = meta.Draft
	view.Prerelease = meta.Prerelease
	view.Created = meta.Created
	for _, b := range a.Blobs {
		view.Assets = append(view.Assets, b.Name)
	}
	return view
}

type labReleaseMeta struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	RevisionID  string    `json:"revision_id"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	Created     time.Time `json:"created"`
}

func (s *server) labListReleases(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	repoName := releaseRepository(repo)
	versions, err := s.registry.Meta.ListVersions(r.Context(), "generic", repoName)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	var out []labReleaseView
	for _, v := range versions {
		art, err := s.registry.Meta.Get(r.Context(), "generic", repoName, v)
		if err != nil {
			continue
		}
		out = append(out, releaseFromArtifact(art))
	}
	labJSON(w, http.StatusOK, out)
}

func (s *server) labCreateRelease(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		var req labCreateReleaseReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		if req.Tag == "" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("tag required"))
			return
		}
		repoName := releaseRepository(repo)
		// Create/replace the artifact record; attachment uploads happen later.
		art := artifactkit.Artifact{
			Format: "generic", Repository: repoName, Version: req.Tag,
		}
		meta := labReleaseMeta{
			Name: req.Name, Description: req.Description, RevisionID: req.RevisionID,
			Draft: req.Draft, Prerelease: req.Prerelease, Created: time.Now().UTC(),
		}
		metaBytes, _ := json.Marshal(meta)
		art.Proprietary = metaBytes
		if err := s.registry.Meta.Put(r.Context(), art); err != nil {
			labErr(w, http.StatusConflict, err)
			return
		}
		labJSON(w, http.StatusOK, releaseFromArtifact(art))
	})(w, r)
}

func (s *server) labDeleteRelease(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		repoName := releaseRepository(repo)
		tag := r.PathValue("tag")
		art, err := s.registry.Meta.Get(r.Context(), "generic", repoName, tag)
		if err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		// Remove any asset blobs, then the artifact record.
		for _, b := range art.Blobs {
			_ = s.registry.Blobs.Delete(r.Context(), b.Digest)
		}
		if err := s.registry.Meta.Delete(r.Context(), "generic", repoName, tag); err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{"deleted": tag})
	})(w, r)
}

type labAssetView struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

func (s *server) labUploadReleaseAsset(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		repoName := releaseRepository(repo)
		tag := r.PathValue("tag")
		if _, err := s.registry.Meta.Get(r.Context(), "generic", repoName, tag); err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		name := header.Filename
		if n := r.FormValue("name"); n != "" {
			name = n
		}
		digest := artifactkit.DigestOf(data)
		if _, err := s.registry.Blobs.PutIfAbsent(r.Context(), digest, bytes.NewReader(data)); err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}
		// Attach the asset descriptor to the artifact (idempotent per name).
		art, _ := s.registry.Meta.Get(r.Context(), "generic", repoName, tag)
		kept := art.Blobs[:0]
		for _, b := range art.Blobs {
			if b.Name != name {
				kept = append(kept, b)
			}
		}
		art.Blobs = append(kept, artifactkit.Descriptor{Digest: digest, Size: int64(len(data)), Name: name})
		if err := s.registry.Meta.Put(r.Context(), art); err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}
		labJSON(w, http.StatusOK, labAssetView{Name: name, Digest: digest, Size: int64(len(data))})
	})(w, r)
}

func (s *server) labListReleaseAssets(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	repoName := releaseRepository(repo)
	art, err := s.registry.Meta.Get(r.Context(), "generic", repoName, r.PathValue("tag"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	var out []labAssetView
	for _, b := range art.Blobs {
		out = append(out, labAssetView{Name: b.Name, Digest: b.Digest, Size: b.Size})
	}
	labJSON(w, http.StatusOK, out)
}

func (s *server) labDownloadReleaseAsset(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	repoName := releaseRepository(repo)
	art, err := s.registry.Meta.Get(r.Context(), "generic", repoName, r.PathValue("tag"))
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	name := r.PathValue("asset")
	var chosen artifactkit.Descriptor
	for _, b := range art.Blobs {
		if b.Name == name {
			chosen = b
			break
		}
	}
	if chosen.IsEmpty() {
		labErr(w, http.StatusNotFound, err)
		return
	}
	rd, err := s.registry.Blobs.Open(r.Context(), chosen.Digest)
	if err != nil || rd == nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	defer rd.Close()
	w.Header().Set("Content-Disposition", "attachment; filename=\""+chosen.Name+"\"")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(chosen.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rd)
}

// ---- merge requests ----

type labMRAuthor struct {
	Username string `json:"username,omitempty"`
}

type labMRView struct {
	IID         int64       `json:"iid"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Source      string      `json:"source"`
	Target      string      `json:"target"`
	State       string      `json:"state"`
	Author      labMRAuthor `json:"author"`
	Created     int64       `json:"created_ms"`
	Updated     int64       `json:"updated_ms"`
}

func (s *server) labListMRs(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	state := r.URL.Query().Get("state")
	mrs, err := s.cs.ListMergeRequests(repo.RepoID(), state)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	var out []labMRView
	for _, m := range mrs {
		view := labMRView{
			IID: m.IID, Title: m.Title, Description: m.Description,
			Source: m.Source, Target: m.Target, State: m.State,
			Created: m.Created.UnixMilli(), Updated: m.Updated.UnixMilli(),
		}
		if m.AuthorID != nil {
			if u, _ := s.cs.GetUser(*m.AuthorID); u != nil {
				view.Author.Username = u.Username
			}
		}
		out = append(out, view)
	}
	labJSON(w, http.StatusOK, out)
}

type labCreateMRReq struct {
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
	Target      string `json:"target"`
}

func (s *server) labCreateMR(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, user, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		var req labCreateMRReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		if req.Title == "" || req.Source == "" || req.Target == "" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("title, source, target required"))
			return
		}
		var authorID *int64
		if user != nil {
			authorID = &user.ID
		}
		mr, err := s.cs.CreateMergeRequest(repo.RepoID(), &store.MergeRequest{
			Title: req.Title, Description: req.Description,
			Source: req.Source, Target: req.Target, State: "open", AuthorID: authorID,
		})
		if err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{"iid": mr.IID, "state": mr.State, "title": mr.Title})
	})(w, r)
}

func (s *server) labGetMR(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	iid, err := parseIID(r)
	if err != nil {
		labErr(w, http.StatusBadRequest, err)
		return
	}
	mr, err := s.cs.GetMergeRequest(repo.RepoID(), iid)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	view := labMRView{
		IID: mr.IID, Title: mr.Title, Description: mr.Description,
		Source: mr.Source, Target: mr.Target, State: mr.State,
		Created: mr.Created.UnixMilli(), Updated: mr.Updated.UnixMilli(),
	}
	if mr.AuthorID != nil {
		if u, _ := s.cs.GetUser(*mr.AuthorID); u != nil {
			view.Author.Username = u.Username
		}
	}
	labJSON(w, http.StatusOK, view)
}

type labUpdateMRReq struct {
	State string `json:"state"`
}

func (s *server) labUpdateMR(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		iid, err := parseIID(r)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		var req labUpdateMRReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		switch req.State {
		case "merge", "merged", "close", "closed", "reopen", "open":
		default:
			labErr(w, http.StatusBadRequest, fmt.Errorf("invalid state %q", req.State))
			return
		}
		state := req.State
		if state == "merged" {
			state = "merged"
		} else if state == "closed" {
			state = "closed"
		} else if state == "open" {
			state = "open"
		}
		if err := s.cs.UpdateMergeRequestState(repo.RepoID(), iid, state); err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{"iid": iid, "state": state})
	})(w, r)
}

func (s *server) labMergeMR(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		iid, err := parseIID(r)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		mr, err := s.cs.GetMergeRequest(repo.RepoID(), iid)
		if err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		ws := revision.NewWorkspace(repo)
		// Single-parent (rebase) merge: fold the source tree into the target
		// tree via a 3-way merge, then record the result as ONE new revision
		// with the target as its sole parent (no merge node). Overlaps become
		// first-class conflict objects.
		srcTree, err := treeOf(ws, repo, mr.Source)
		if err != nil {
			labErr(w, http.StatusBadRequest, fmt.Errorf("source %s: %w", mr.Source, err))
			return
		}
		tgtSnap, err := resolveSnapshotOf(ws, repo, mr.Target)
		if err != nil {
			labErr(w, http.StatusBadRequest, fmt.Errorf("target %s: %w", mr.Target, err))
			return
		}
		var base object.ID
		if len(tgtSnap.Parents) > 0 {
			base = tgtSnap.Parents[0]
		}
		mergedID, atoms, err := ws.Merge(base, srcTree, tgtSnap.TreeID)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		newSnap, newRev, err := ws.Commit(revision.CommitParams{
			Parents: []object.ID{tgtSnap.RevisionHash}, TreeID: mergedID,
			Description: "Merge " + mr.Source + " into " + mr.Target,
			Author:      store.Author{Name: "easyvcs", Email: "easyvcs@example.com"},
		})
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		_ = newRev
		if err := s.cs.UpdateMergeRequestState(repo.RepoID(), iid, "merged"); err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{
			"iid": iid, "state": "merged", "revision_id": newSnap.RevisionID,
			"snapshot": newSnap.RevisionHash.String(), "conflicts": len(atoms),
		})
	})(w, r)
}

type labReviewReq struct {
	State string `json:"state"`
	Body  string `json:"body,omitempty"`
}

func (s *server) labAddReview(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, user, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		iid, err := parseIID(r)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		mr, err := s.cs.GetMergeRequest(repo.RepoID(), iid)
		if err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		var req labReviewReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		var reviewerID *int64
		if user != nil {
			reviewerID = &user.ID
		}
		rv, err := s.cs.AddReview(mr.ID, reviewerID, req.State, req.Body)
		if err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{"review": rv.State})
	})(w, r)
}

func (s *server) labListReviews(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	iid, err := parseIID(r)
	if err != nil {
		labErr(w, http.StatusBadRequest, err)
		return
	}
	mr, err := s.cs.GetMergeRequest(repo.RepoID(), iid)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	reviews, err := s.cs.ListReviews(mr.ID)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	labJSON(w, http.StatusOK, reviews)
}

type labCommentReq struct {
	Body string `json:"body"`
	Path string `json:"path,omitempty"`
}

func (s *server) labAddComment(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, user, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		iid, err := parseIID(r)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		mr, err := s.cs.GetMergeRequest(repo.RepoID(), iid)
		if err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		var req labCommentReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		var authorID *int64
		if user != nil {
			authorID = &user.ID
		}
		c, err := s.cs.AddComment(mr.ID, authorID, req.Body, req.Path)
		if err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{"comment": c.ID, "body": c.Body})
	})(w, r)
}

func (s *server) labListComments(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	iid, err := parseIID(r)
	if err != nil {
		labErr(w, http.StatusBadRequest, err)
		return
	}
	mr, err := s.cs.GetMergeRequest(repo.RepoID(), iid)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	comments, err := s.cs.ListComments(mr.ID)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	labJSON(w, http.StatusOK, comments)
}
