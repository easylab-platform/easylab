package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"easyvcs/internal/mirror"
	"easyvcs/internal/store"
)

// ---- push-mirror CRUD (normal, read-write repos) ----

type labPushMirrorReq struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Branch string `json:"branch"`
	Token  string `json:"token"`
}

func (s *server) labListPushMirrors(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	list, err := repo.ListPushMirrors()
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]map[string]any, 0, len(list))
	for _, m := range list {
		out = append(out, labPushMirrorView(m))
	}
	labJSON(w, http.StatusOK, out)
}

func (s *server) labCreatePushMirror(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		var req labPushMirrorReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		if req.Name == "" || req.URL == "" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("name and url required"))
			return
		}
		branch := req.Branch
		if branch == "" {
			if meta, err := repo.RepoMeta(); err == nil {
				branch = meta.DefaultBranch
			}
			if branch == "" {
				branch = "main"
			}
		}
		m := &store.PushMirror{Name: req.Name, URL: req.URL, Branch: branch, Token: req.Token}
		if err := repo.PutPushMirror(m); err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}
		labJSON(w, http.StatusOK, labPushMirrorView(m))
	})(w, r)
}

func (s *server) labDeletePushMirror(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		if err := repo.DeletePushMirror(r.PathValue("name")); err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		labJSON(w, http.StatusOK, labResponse{"deleted": r.PathValue("name")})
	})(w, r)
}

func (s *server) labPushMirror(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		m, err := repo.GetPushMirror(r.PathValue("name"))
		if err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		res, err := mirror.Push(r.Context(), repo, mirror.PushTarget{
			Name: m.Name, URL: m.URL, Branch: m.Branch, Token: m.Token, LastRev: m.LastRev,
		})
		if err != nil {
			_ = repo.UpdatePushMirrorSync(m.Name, m.LastRev, err.Error())
			labErr(w, http.StatusBadRequest, err)
			return
		}
		_ = repo.UpdatePushMirrorSync(m.Name, res.RevID, "")
		labJSON(w, http.StatusOK, labResponse{"mirror": m.Name, "rev": res.RevID, "tree": res.TreeID})
	})(w, r)
}

// labPullMirror is only valid on a mirror repo: refresh the read-only snapshot
// from the external source. On a normal repo it is a no-op guard.
func (s *server) labPullMirror(w http.ResponseWriter, r *http.Request) {
	s.labMirrorRepoPull(w, r)
}

// labSyncMirror dispatches by repo kind: mirror repo -> pull; normal -> push.
func (s *server) labSyncMirror(w http.ResponseWriter, r *http.Request) {
	repo, _, ok := s.labRepo(w, r)
	if !ok {
		return
	}
	if repo.IsMirror() {
		s.labMirrorRepoPull(w, r)
		return
	}
	s.labPushMirror(w, r)
}

// labMirrorRepoPull performs a manual pull of a read-only mirror repo.
func (s *server) labMirrorRepoPull(w http.ResponseWriter, r *http.Request) {
	s.labRequireWriteMirrorControl(func(w http.ResponseWriter, r *http.Request) {
		repo, _, ok := s.labRepo(w, r)
		if !ok {
			return
		}
		meta, err := repo.RepoMeta()
		if err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}
		if meta.Kind != "mirror" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("not a mirror repository"))
			return
		}
		rev, err := mirror.Pull(r.Context(), repo, mirror.PullConfig{
			URL: meta.MirrorURL, Branch: meta.MirrorBranch, Token: meta.MirrorToken,
		})
		if err != nil {
			_ = repo.TouchMirrorSync("", time.Now().UTC().UnixMilli(), err.Error())
			labErr(w, http.StatusBadRequest, err)
			return
		}
		_ = repo.TouchMirrorSync(rev.ID, time.Now().UTC().UnixMilli(), "")
		labJSON(w, http.StatusOK, labResponse{"rev": rev.ID, "tree": rev.Hash.String()})
	})(w, r)
}

// labPushMirrorView strips the token before returning a push mirror.
func labPushMirrorView(m *store.PushMirror) map[string]any {
	return map[string]any{
		"name": m.Name, "url": m.URL, "branch": m.Branch,
		"last_rev": m.LastRev, "last_error": m.LastError,
	}
}
