package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/easylab-platform/easylab/internal/k8s"
	"github.com/easylab-platform/easylab/internal/ops"
	"github.com/easylab-platform/easyvcs/store"
)

// opsState bundles all runtime/registry state the /ops handlers need.
type opsState struct {
	builders   *ops.TaskRegistry
	namespaces *ops.NamespaceRegistry
	services   ops.ServiceRunner
}

// newOpsState wires the /ops substrate: image builds + container services now
// run on Kubernetes (no privileged podman sidecar). The k8s client is created
// separately (main) so startup can degrade gracefully when not in-cluster.
func newOpsState() (*opsState, error) {
	return &opsState{
		builders:   ops.NewTaskRegistry(),
		namespaces: ops.FromEnv(),
	}, nil
}

type opsBuildReq struct {
	Context       string   `json:"context,omitempty"`
	Containerfile string   `json:"containerfile,omitempty"`
	Dockerfile    string   `json:"dockerfile,omitempty"`
	Image         string   `json:"image"`
	Registry      string   `json:"registry,omitempty"`
	RegistryAuth  string   `json:"registry_auth,omitempty"`
	CacheRepo     string   `json:"cache_repo,omitempty"`
	BuildArgs     []string `json:"build_args,omitempty"`
	NoCache       bool     `json:"no_cache,omitempty"`
	TimeoutS      int      `json:"timeout_secs,omitempty"`
}

// --- routes ---

func (s *server) opsRouter() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/v1/ops/namespaces", s.opsNamespaces)
	m.HandleFunc("GET /api/v1/ops/tasks", s.opsTasksList)
	m.HandleFunc("GET /api/v1/ops/tasks/{id}", s.opsTaskGet)
	m.HandleFunc("GET /api/v1/ops/tasks/{id}/stream", s.opsTaskStream)
	m.HandleFunc("POST /api/v1/ops/builds", s.opsBuild)
	return m
}

func (s *server) opsNamespaces(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"namespaces": s.ops.namespaces.List(),
		"default":    s.ops.namespaces.Default(),
	})
}

func (s *server) opsTasksList(w http.ResponseWriter, r *http.Request) {
	tasks := s.ops.builders.List()
	var out []map[string]any
	for _, t := range tasks {
		snap := t.Snapshot()
		out = append(out, map[string]any{
			"id": t.ID, "kind": t.Kind, "state": snap.State,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) opsTaskGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task := s.ops.builders.Get(id)
	if task == nil {
		labErr(w, http.StatusNotFound, fmt.Errorf("task not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": task.ID, "kind": task.Kind, "state": task.State(),
	})
}

func (s *server) opsTaskStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task := s.ops.builders.Get(id)
	if task == nil {
		labErr(w, http.StatusNotFound, fmt.Errorf("task not found"))
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		labErr(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	ch, cancel := task.Subscribe()
	defer cancel()
	enc := json.NewEncoder(w)
	for ev := range ch {
		if err := enc.Encode(ev); err != nil {
			return
		}
		fl.Flush()
	}
}

func (s *server) opsBuild(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		var req opsBuildReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		if req.Image == "" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("image required"))
			return
		}
		if s.k8s == nil {
			labErr(w, http.StatusNotImplemented, fmt.Errorf("k8s backend unavailable"))
			return
		}
		id := s.ops.builders.NewID("build")
		task := s.ops.builders.Create(id, ops.KindBuild)
		ctxDir := req.Context
		dockerfile := req.Dockerfile
		image := req.Image
		buildArgs := map[string]string{}
		for _, a := range req.BuildArgs {
			if i := strings.Index(a, "="); i > 0 {
				buildArgs[a[:i]] = a[i+1:]
			}
		}
		go func() {
			err := s.k8s.BuildImage(context.Background(), k8s.BuildOptions{
				ContextDir: ctxDir, Dockerfile: dockerfile, Image: image,
				BuildArgs: buildArgs, NoCache: req.NoCache,
			}, func(line string) { task.Log(line) })
			if err != nil {
				task.Finish(false, "", err.Error())
				return
			}
			task.Finish(true, "image="+image, "")
		}()
		writeJSON(w, http.StatusOK, map[string]any{"build_id": id})
	})(w, r)
}

// registryRoot returns the EasyVCS home registry directory path (used as the
// ops work root for scratch spaces).
func registryRoot() string {
	return store.HomeDir() + "/registry"
}

// ---- services (host docker engine socket backend) ----

func (s *server) opsServicesList(w http.ResponseWriter, r *http.Request) {
	if s.ops.services == nil {
		labErr(w, http.StatusNotImplemented, fmt.Errorf("services backend unavailable"))
		return
	}
	net := r.URL.Query().Get("network")
	list, err := s.ops.services.List(r.Context(), net)
	if err != nil {
		labErr(w, http.StatusInternalServerError, err)
		return
	}
	labJSON(w, http.StatusOK, list)
}

func (s *server) opsServiceLaunch(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		if s.ops.services == nil {
			labErr(w, http.StatusNotImplemented, fmt.Errorf("services backend unavailable"))
			return
		}
		var req ops.ServiceRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		if s.k8s != nil {
			req.Image = k8s.RewriteImageRef(req.Image, s.k8s.RegistryHost())
		}
		st, err := s.ops.services.Launch(r.Context(), req, nil)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		labJSON(w, http.StatusOK, st)
	})(w, r)
}

func (s *server) opsServiceGet(w http.ResponseWriter, r *http.Request) {
	if s.ops.services == nil {
		labErr(w, http.StatusNotImplemented, fmt.Errorf("services backend unavailable"))
		return
	}
	name := r.PathValue("name")
	st, err := s.ops.services.Status(r.Context(), name)
	if err != nil {
		labErr(w, http.StatusNotFound, err)
		return
	}
	labJSON(w, http.StatusOK, st)
}

func (s *server) opsServiceDelete(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		if s.ops.services == nil {
			labErr(w, http.StatusNotImplemented, fmt.Errorf("services backend unavailable"))
			return
		}
		name := r.PathValue("name")
		if err := s.ops.services.Delete(r.Context(), name); err != nil {
			labErr(w, http.StatusInternalServerError, err)
			return
		}
		labJSON(w, http.StatusOK, map[string]any{"deleted": name})
	})(w, r)
}

type opsScaleReq struct {
	Replicas int `json:"replicas"`
}

func (s *server) opsServiceScale(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		if s.ops.services == nil {
			labErr(w, http.StatusNotImplemented, fmt.Errorf("services backend unavailable"))
			return
		}
		name := r.PathValue("name")
		var req opsScaleReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		st, err := s.ops.services.Scale(r.Context(), name, req.Replicas)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		labJSON(w, http.StatusOK, st)
	})(w, r)
}

// ---- sandbox pass-through (deprecated; use SandboxService) ----

func (s *server) opsSandboxExec(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		labErr(w, http.StatusNotImplemented, fmt.Errorf("use SandboxService.Execute"))
	})(w, r)
}

func (s *server) opsSandboxReadFile(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		labErr(w, http.StatusNotImplemented, fmt.Errorf("use SandboxService.FileRead"))
	})(w, r)
}

func (s *server) opsSandboxWriteFile(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		labErr(w, http.StatusNotImplemented, fmt.Errorf("use SandboxService.FileWrite"))
	})(w, r)
}
