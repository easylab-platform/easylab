package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"os"
	"sync"

	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
	"github.com/abcp-sdk/abc-protocol-go/extension"
	"github.com/abcp-sdk/abc-protocol-go/manifest"
	natsbus "github.com/abcp-sdk/abc-protocol-go/transport/nats"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"easyvcs-ext-ops/internal/easylab"
	"easyvcs-ext-ops/internal/worker"
)

//go:embed manifest.yaml
var manifestYaml []byte

type server struct {
	jjops             *easylab.Client // easylab /ops client (owns all k8s access)
	workerImage       string          // sandbox worker image (easylab runs it)
	runtimeNamespace  string          // namespace where easylab creates sandboxes/deployments
	ext               *extension.Extension
	artifact          string // artifact registry base URL (packages + OCI + metadata)
	artifactImageHost string // TLS ingress host for image refs (FROM/push via buildkit)
	artifactToken     string // optional bearer/basic token for artifact write auth
	jj                string // easylab URL (repo archive + contents + clone)
	jjToken           string // easylab write token (Authorization: token <…>)

	wsMu    sync.Mutex              // guards wsCache
	wsCache map[string]wsCacheEntry // session -> workspace (short TTL)

	syncMu sync.Mutex        // guards synced
	synced map[string]string // container key -> synced rev

	// workerResolver overrides worker URL resolution (tests).
	workerResolver func(cid string) (string, error)

	builds sync.Map // build id -> *buildTask
}

func main() {
	ns := envOr("ZERGX_K8S_NAMESPACE", "zergx")
	img := envOr("ZERGX_WORKER_IMAGE", "jj-lab.temp.svc.cluster.local/zergx-worker:v0.0.1")
	natsURL := envOr("NATS_URL", "nats://nats.zergx.svc.cluster.local:4222")
	port := envOr("ZERGX_PORT", "8080")
	// easylab replaces the old repo-manager (archive + contents + clone).
	// The cluster service is named repo (easylab is the binary).
	jj := envOr("ZERGX_JJ_SERVER_URL", envOr("ZERGX_REPO_MANAGER_URL", "http://jj-lab.temp.svc.cluster.local:80"))
	// Artifact registry replaces zot (OCI store) + the legacy registry (metadata):
	// one base URL serves /v2 (OCI), /pkgs/<format> (protocol proxies) and
	// /pkgs/system (admin/metadata). This is the plain-HTTP in-cluster base
	// used for API calls and in-container CLI uploads.
	artifact := trimTrailingSlash(envOr("ZERGX_ARTIFACT_URL", "http://jj-lab.temp.svc.cluster.local"))
	// Image references (buildkit FROM/push) must go through the TLS ingress
	// host configured as insecure in buildkitd's registry config — the svc
	// host is plain HTTP which buildkit cannot pull/push to.
	artifactImageHost := envOr("ZERGX_ARTIFACT_IMAGE_HOST", "jj-lab.temp.svc.cluster.local")
	artifactToken := envOr("ZERGX_ARTIFACT_TOKEN", "")
	jjToken := envOr("JJLAB_TOKEN", envOr("ZERGX_JJLAB_TOKEN", "devtoken"))

	runtimeNS := envOr("ZERGX_RUNTIME_NAMESPACE", ns)

	s := &server{
		jjops:             easylab.New(jj, jjToken),
		artifact:          artifact,
		artifactImageHost: artifactImageHost,
		artifactToken:     artifactToken,
		jj:                jj,
		jjToken:           jjToken,
		workerImage:       img,
		runtimeNamespace:  runtimeNS,
		wsCache:           map[string]wsCacheEntry{},
		synced:            map[string]string{},
	}

	// Verification instances must set ZERGX_DISABLE_NATS=1. Tool-call and
	// variable subscriptions use queue groups keyed by the extension id, so a
	// second replica with the same id would STEAL live tool calls away from
	// the serving instance (and double-answer abc.discover, which has no
	// queue group by design). To keep the tools testable without joining the
	// bus, such instances expose them over HTTP at POST /api/v1/tools/{name}
	// with a JSON args body.
	toolBridge := false
	if os.Getenv("ZERGX_DISABLE_NATS") != "1" {
		nbus, err := natsbus.Connect(natsURL)
		if err != nil {
			slog.Error("nats connect failed", "svc", "ops-extension", "err", err)
			os.Exit(1)
		}
		m, err := manifest.ParseManifest(manifestYaml)
		if err != nil {
			slog.Error("load manifest failed", "svc", "ops-extension", "err", err)
			os.Exit(1)
		}

		r := s.router(toolBridge)

		if err := extension.Serve(
			extension.New(nbus, m.BuildConfig(manifest.Bindings{
				Handlers: s.handlers(),
				Variables: map[string]extension.VariableSpec{
					"sandbox-id":     {Resolve: s.resolveSandboxID},
					"sandbox-status": {Resolve: s.resolveSandboxStatus},
				},
				OnLifecycle: func(ctx context.Context, ev abcprotocol.LifecycleEvent) error {
					if ev.Kind == "deleted" {
						s.clearSandboxVars(ctx, ev.SessionName)
					}
					return nil
				},
			})),
			extension.ServeOptions{
				Handler: r,
				Port:    port,
				Run: func(runCtx context.Context, ext *extension.Extension) {
					s.ext = ext
					slog.Info("listening", "svc", "ops-extension", "addr", ":"+port, "artifact", artifact, "jj", jj, "runtime-ns", runtimeNS)
				},
			},
		); err != nil {
			slog.Error("serve failed", "svc", "ops-extension", "err", err)
			os.Exit(1)
		}
		return
	}

	r := s.router(true)
	addr := ":" + port
	slog.Info("listening", "svc", "ops-extension", "addr", addr, "artifact", artifact, "jj", jj, "runtime-ns", runtimeNS)
	if err := http.ListenAndServe(addr, r); err != nil {
		slog.Error("http server failed", "svc", "ops-extension", "err", err)
		os.Exit(1)
	}
}

// router builds the chi router serving both the ops API and the embedded SPA.
// `toolBridge` exposes the NATS tools over HTTP for verification instances.
func (s *server) router(toolBridge bool) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/health", s.health)
		r.Get("/sandboxes", s.sandboxesList)
		r.Get("/sandboxes/{session}", s.sandboxGet)
		r.Delete("/sandboxes/{session}", s.deleteContainer)
		r.Post("/sandboxes/{session}/exec", s.exec)
		r.Get("/sandboxes/{session}/jobs", s.listJobs)
		r.Get("/sandboxes/{session}/jobs/{jobID}/output", s.jobOutput)
		r.Post("/sandboxes/{session}/jobs/{jobID}/wait", s.jobWait)
		r.Post("/sandboxes/{session}/jobs/{jobID}/stdin", s.jobStdin)
		r.Post("/sandboxes/{session}/jobs/{jobID}/kill", s.kill)
		r.Post("/sandboxes/{session}/read", s.sandboxRead)
		r.Post("/sandboxes/{session}/write", s.sandboxWrite)
		r.Get("/sandboxes/{session}/ws", s.wsProxy)
		r.Get("/sandboxes/{session}/ws/job", s.wsProxyJob)
		r.Post("/deployments", s.deploy)
		r.Get("/infra/k8s/config", s.k8sConfig)
		r.Post("/images/build", s.buildImage)
		r.Get("/builds", s.buildsList)
		r.Get("/builds/{id}", s.buildGet)
		r.Get("/builds/{id}/stream", s.buildStream)
		r.Get("/containerfile-templates", s.containerfileTemplates)
		r.Get("/status", s.status)
		r.Get("/deployments", s.deploymentsList)
		r.Get("/deployments/{name}/pods", s.deploymentPods)
		r.Get("/deployments/{name}/status", s.deploymentStatus)
		r.Post("/deployments/{name}/restart", s.deploymentRestart)
		r.Post("/deployments/{name}/scale", s.deploymentScale)
		r.Post("/deployments/{name}/rollback", s.deploymentRollback)
		r.Get("/deployments/{name}/events", s.deploymentEvents)
		r.Get("/deployments/{name}/revisions", s.deploymentRevisions)
		r.Delete("/deployments/{name}", s.deploymentDelete)
		r.Get("/packages", s.packagesList)
		r.Get("/images", s.imagesList)
		r.Get("/publish-specs", s.publishSpecsHandler)
		r.Post("/packages/publish", s.packagesPublish)
		if toolBridge {
			r.Post("/tools/{name}", s.callTool)
		}
	})

	// Embedded SPA (served at /; /api/v1 routes registered above win).
	r.Handle("/*", spaHandler())
	return r
}

// callTool bridges a NATS tool over HTTP for verification instances
// (ZERGX_DISABLE_NATS=1). Body: JSON object of tool args.
func (s *server) callTool(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	spec, ok := s.handlers()[name]
	if !ok {
		writeErr(w, http.StatusNotFound, "no such tool: "+name)
		return
	}
	args := map[string]interface{}{}
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil && err.Error() != "EOF" {
		writeErr(w, http.StatusBadRequest, "invalid args body: "+err.Error())
		return
	}
	res, err := spec.Execute(r.Context(), args, "http-verify", "")
	out := res.Content
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "result": out})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]interface{}{"ok": false, "error": msg})
}

// resolveWorkerURL finds the worker URL for a container ID (easylab-sourced).
func (s *server) resolveWorkerURL(ctx context.Context, cid string) (string, error) {
	info, err := s.workerInfo(ctx, cid)
	if err != nil {
		return "", fmt.Errorf("no worker for container %s", cid)
	}
	if info.WorkerURL == "" {
		return "", fmt.Errorf("worker not ready")
	}
	return info.WorkerURL, nil
}

func (s *server) workerCommand(ctx context.Context, cid, method string, params map[string]interface{}) (interface{}, error) {
	wu, err := s.resolveWorkerURL(ctx, cid)
	if err != nil {
		return nil, err
	}
	return worker.CommandOnce(ctx, worker.ToWsURL(wu), method, params)
}
