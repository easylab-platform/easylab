package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"net/http"
	"os"
	"strings"
	"time"

	"easyvcs/internal/ops"
	"easyvcs/internal/store"
)

// opsState bundles all runtime/registry state the /ops handlers need.
type opsState struct {
	builders   *ops.TaskRegistry
	namespaces *ops.NamespaceRegistry
	runtime    ops.Runtime
	builder    ops.Builder
	services   ops.ServiceRunner
}

// registryHost returns the internal registry host buildah/docker should talk to
// when no explicit registry is supplied. Defaults to the loopback /v2 (the same
// pod serves it); override with EASYVCS_REGISTRY (e.g. a service DNS name when
// EasyLab is a multi-pod deployment).
func registryHost() string {
	if v := os.Getenv("EASYVCS_REGISTRY"); v != "" {
		return v
	}
	return "127.0.0.1:8080"
}

// newOpsState wires the /ops substrate from environment. The build backend is
// embedded buildah (daemonless, rootless). The ONLY service backend is the
// internal podman runner (fully self-contained): each service runs as a podman
// container inside EasyLab — per-service/group network (embedded DNS), real
// cgroup resource limits, port publishing. Requires the pod to mount
// /sys/fs/cgroup read-write (privileged + a startup remount).
func newOpsState() (*opsState, error) {
	workRoot := registryRoot() + "/ops"
	return &opsState{
		builders:   ops.NewTaskRegistry(),
		namespaces: ops.FromEnv(),
		runtime:    ops.NewLocalRuntime(workRoot),
		builder:    ops.NewBuilderFromEnv(),
		services:   newServiceRunner(),
	}, nil
}

// newServiceRunner returns the sole service backend: internal podman.
func newServiceRunner() ops.ServiceRunner {
	workRoot := registryRoot() + "/podman"
	_ = os.MkdirAll(workRoot, 0o755)
	return ops.NewPodmanServiceRunner(registryHost(), workRoot)
}

// opsArgs is a small typed body for runs/builds.
type opsRunReq struct {
	Command string   `json:"command"`
	WorkDir string   `json:"workdir,omitempty"`
	Env     []string `json:"env,omitempty"`
	Timeout int      `json:"timeout_secs,omitempty"`
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
	m.HandleFunc("POST /api/v1/ops/runs", s.opsRun)
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

func (s *server) opsRun(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		var req opsRunReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		if req.Command == "" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("command required"))
			return
		}
		ns, ok := s.ops.namespaces.Resolve("")
		if !ok {
			labErr(w, http.StatusForbidden, fmt.Errorf("no approved namespace"))
			return
		}
		_ = ns
		id := s.ops.builders.NewID("run")
		task := s.ops.builders.Create(id, ops.KindRun)

		spec := ops.RunSpec{
			Command: req.Command,
			WorkDir: req.WorkDir,
			Env:     req.Env,
			Timeout: time.Duration(req.Timeout) * time.Second,
		}
		go func() {
			res, err := s.ops.runtime.Run(context.Background(), spec, func(line string) {
				task.Log(line)
			})
			if err != nil {
				task.Finish(false, "", err.Error())
				return
			}
			task.Finish(res.ExitCode == 0, fmt.Sprintf("exit=%d", res.ExitCode), "")
		}()

		writeJSON(w, http.StatusOK, map[string]any{"run_id": id})
	})(w, r)
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
		id := s.ops.builders.NewID("build")
		task := s.ops.builders.Create(id, ops.KindBuild)

		if req.Registry == "" {
			req.Registry = registryHost()
		}
		if req.CacheRepo == "" {
			// Default cache-repo = the image repository (sans tag), so layers
			// land in the same registry repo and are reused on next builds.
			if i := strings.LastIndex(req.Image, ":"); i > 0 {
				req.CacheRepo = req.Image[:i]
			}
		}

		spec := ops.BuildSpec{
			Context:       req.Context,
			Containerfile: req.Containerfile,
			Dockerfile:    req.Dockerfile,
			Image:         req.Image,
			Registry:      req.Registry,
			RegistryAuth:  req.RegistryAuth,
			CacheRepo:     req.CacheRepo,
			BuildArgs:     req.BuildArgs,
			NoCache:       req.NoCache,
			Timeout:       time.Duration(req.TimeoutS) * time.Second,
		}
		go func() {
			res, err := s.ops.builder.Build(context.Background(), spec, func(line string) {
				task.Log(line)
			})
			if err != nil {
				task.Finish(false, res.Out, err.Error())
				return
			}
			task.Finish(true, fmt.Sprintf("image=%s", res.Image), "")
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

// ---- sandbox pass-through (persistent podman container; no repo commit) ----

// sandboxRunner returns the podman runner (with Exec/ContainerFile) if
// available, else nil.
func (s *server) sandboxRunner() *ops.PodmanServiceRunner {
	if pr, ok := s.ops.services.(*ops.PodmanServiceRunner); ok {
		return pr
	}
	return nil
}

type opsSandboxExecReq struct {
	Command string `json:"command"`
}

func (s *server) opsSandboxExec(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		pr := s.sandboxRunner()
		if pr == nil {
			labErr(w, http.StatusNotImplemented, fmt.Errorf("sandbox backend unavailable"))
			return
		}
		var req opsSandboxExecReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		if req.Command == "" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("command required"))
			return
		}
		out, err := pr.Exec(r.Context(), r.PathValue("name"), req.Command)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		labJSON(w, http.StatusOK, map[string]any{"output": out})
	})(w, r)
}

func (s *server) opsSandboxReadFile(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		pr := s.sandboxRunner()
		if pr == nil {
			labErr(w, http.StatusNotImplemented, fmt.Errorf("sandbox backend unavailable"))
			return
		}
		path := r.URL.Query().Get("path")
		if path == "" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("path required"))
			return
		}
		data, err := pr.ReadContainerFile(r.Context(), r.PathValue("name"), path)
		if err != nil {
			labErr(w, http.StatusNotFound, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
	})(w, r)
}

func (s *server) opsSandboxWriteFile(w http.ResponseWriter, r *http.Request) {
	s.labRequireWrite(func(w http.ResponseWriter, r *http.Request) {
		pr := s.sandboxRunner()
		if pr == nil {
			labErr(w, http.StatusNotImplemented, fmt.Errorf("sandbox backend unavailable"))
			return
		}
		path := r.URL.Query().Get("path")
		if path == "" {
			labErr(w, http.StatusBadRequest, fmt.Errorf("path required"))
			return
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		if err := pr.ContainerFile(r.Context(), r.PathValue("name"), path, data); err != nil {
			labErr(w, http.StatusBadRequest, err)
			return
		}
		labJSON(w, http.StatusOK, map[string]any{"written": path})
	})(w, r)
}
