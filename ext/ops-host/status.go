package main

import (
	"context"
	"encoding/json"
	"net/http"

	"sort"
	"time"

	"github.com/go-chi/chi/v5"
)

const version = "0.2.0"

// status reports service dependencies' health for the frontend overview.
func (s *server) status(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	check := func(name, url string) map[string]interface{} {
		cctx, ccancel := context.WithTimeout(ctx, 4*time.Second)
		defer ccancel()
		req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
		if err != nil {
			return map[string]interface{}{"name": name, "ok": false, "error": err.Error()}
		}
		resp, err := defaultClient.Do(req)
		if err != nil {
			return map[string]interface{}{"name": name, "ok": false, "error": err.Error()}
		}
		defer resp.Body.Close()
		return map[string]interface{}{"name": name, "ok": resp.StatusCode < 300, "status": resp.StatusCode}
	}

	deps := []map[string]interface{}{
		check("artifact", s.artifact+"/v2/"),
		check("easylab", s.base+"/api/v1/health"),
	}
	_, cfgErr := s.ops.Config(ctx)
	deps = append(deps, map[string]interface{}{"name": "easylab-ops", "ok": cfgErr == nil, "error": errStr(cfgErr)})

	svcs, _ := s.ops.ListServices(ctx, s.runtimeNamespace)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":        true,
		"version":   version,
		"deps":      deps,
		"sandboxes": len(svcs),
	})
}

// errStr renders an error or "".
func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// sandboxesList returns worker pods with their session labels and the repo rev
// each is synced to.
func (s *server) sandboxesList(w http.ResponseWriter, r *http.Request) {
	list, err := s.ops.ListServices(r.Context(), s.runtimeNamespace)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.syncMu.Lock()
	synced := make(map[string]string, len(s.synced))
	for k, v := range s.synced {
		synced[k] = v
	}
	s.syncMu.Unlock()

	out := []map[string]interface{}{}
	for _, svc := range list {
		name, _ := svc["name"].(string)
		// The session association lives in the `zergx/session` annotation, not
		// in a top-level `session` field (easylab ListServices returns the
		// annotations verbatim). Read it the same way filterServicesBySession
		// does, so the UI can bind a sandbox to its session.
		session := ""
		if ann, ok := svc["annotations"].(map[string]interface{}); ok {
			if s, ok := ann["zergx/session"].(string); ok {
				session = s
			}
		}
		out = append(out, map[string]interface{}{
			"container_id": name,
			"session":      session,
			"pod_name":     name,
			"status":       svc["phase"],
			"pod_ip":       svc["pod_ip"],
			"synced_rev":   synced[name],
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"sandboxes": out})
}

// sandboxGet returns one session's sandbox pod plus the deployments it owns.
func (s *server) sandboxGet(w http.ResponseWriter, r *http.Request) {
	session := chi.URLParam(r, "session")
	key := sessionKey(session)
	info, err := s.workerInfo(r.Context(), key)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}

	s.syncMu.Lock()
	syncedRev := s.synced[info.ContainerID]
	s.syncMu.Unlock()

	sandbox := map[string]interface{}{
		"container_id": info.ContainerID,
		"session":      info.SessionName,
		"pod_name":     info.PodName,
		"status":       info.Status,
		"worker_url":   info.WorkerURL,
		"pod_ip":       info.PodIP,
		"synced_rev":   syncedRev,
	}

	svcs, err := s.ops.ListServices(r.Context(), s.runtimeNamespace)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	outDeps := []map[string]interface{}{}
	for _, svc := range svcs {
		if svc["kind"] != "deployment" {
			continue
		}
		if svcSession, _ := svc["session"].(string); svcSession != "" && svcSession != session {
			continue
		}
		outDeps = append(outDeps, svc)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"sandbox":     sandbox,
		"deployments": outDeps,
	})
}

// deploymentsList returns the deployments (services) this ops-extension owns.
func (s *server) deploymentsList(w http.ResponseWriter, r *http.Request) {
	list, err := s.ops.ListServices(r.Context(), s.runtimeNamespace)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]interface{}{}
	for _, svc := range list {
		if svc["kind"] == "deployment" {
			out = append(out, svc)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deployments": out})
}

// deploymentPods returns the pods of one deployment.
func (s *server) deploymentPods(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	pods, err := s.ops.ServicePods(r.Context(), name, s.runtimeNamespace)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"pods": pods})
}

// deploymentStatus reports the rollout state of one deployment.
func (s *server) deploymentStatus(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	st, err := s.ops.Service(r.Context(), name, s.runtimeNamespace)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":     st.Name,
		"kind":     st.Kind,
		"replicas": st.Replicas,
		"ready":    st.ReadyOK(),
		"phase":    st.Phase,
		"pod_ip":   st.PodIP,
	})
}

// deploymentDelete removes a deployment + service.
func (s *server) deploymentDelete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.ops.DeleteService(r.Context(), name, s.runtimeNamespace); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// deploymentRestart triggers a rolling restart (bumps the restartedAt
// annotation on the pod template).
func (s *server) deploymentRestart(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if err := s.ops.RestartService(r.Context(), name, s.runtimeNamespace); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// deploymentScale sets the replica count.
func (s *server) deploymentScale(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var b struct {
		Replicas int32 `json:"replicas"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	if err := s.ops.ScaleService(r.Context(), name, int(b.Replicas), s.runtimeNamespace); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "replicas": b.Replicas})
}

// deploymentRollback rolls back to a previous revision (0 = previous).
func (s *server) deploymentRollback(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	var b struct {
		Revision int64 `json:"revision"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	if err := s.ops.RollbackService(r.Context(), name, b.Revision, s.runtimeNamespace); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// deploymentEvents lists k8s events for a deployment (rollout debugging).
func (s *server) deploymentEvents(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	events, err := s.ops.ServiceEvents(r.Context(), name, s.runtimeNamespace)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"events": events})
}

// deploymentRevisions lists the ReplicaSet revisions of a deployment.
func (s *server) deploymentRevisions(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	revs, err := s.ops.ServiceRevisions(r.Context(), name, s.runtimeNamespace)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"revisions": revs})
}

// packagesList proxies the artifact registry's package list (avoids CORS and
// keeps the artifact URL internal to the cluster).
func (s *server) packagesList(w http.ResponseWriter, r *http.Request) {
	body, err := s.httpGetRaw(r.Context(), s.artifact+"/pkgs/system/packages")
	if err != nil {
		writeErr(w, http.StatusBadGateway, "artifact: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// imagesList proxies the artifact OCI catalog (GET /v2/_catalog).
func (s *server) imagesList(w http.ResponseWriter, r *http.Request) {
	body, err := s.httpGetRaw(r.Context(), s.artifact+"/v2/_catalog")
	if err != nil {
		writeErr(w, http.StatusBadGateway, "artifact: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// publishSpecsHandler exports the per-protocol publish metadata so the
// frontend can render a dynamic publish form.
func (s *server) publishSpecsHandler(w http.ResponseWriter, r *http.Request) {
	type specOut struct {
		Protocol string   `json:"protocol"`
		Args     []string `json:"args"`
		Required []string `json:"required"`
	}
	out := make([]specOut, 0, len(publishSpecs))
	for proto, spec := range publishSpecs {
		out = append(out, specOut{Protocol: proto, Args: spec.args, Required: spec.required})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Protocol < out[j].Protocol })
	writeJSON(w, http.StatusOK, map[string]interface{}{"specs": out})
}

// packagesPublish is the HTTP face of the package-publish tool.
func (s *server) packagesPublish(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Protocol   string `json:"protocol"`
		Org        string `json:"org"`
		Repo       string `json:"repo"`
		Branch     string `json:"branch"`
		Session    string `json:"session"`
		Name       string `json:"name"`
		Version    string `json:"version"`
		File       string `json:"file"`
		Dockerfile string `json:"dockerfile-path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if b.Protocol == "" {
		writeErr(w, http.StatusBadRequest, "protocol required")
		return
	}

	id := s.startPublishTask(publishTaskBody{
		Protocol:   b.Protocol,
		Org:        b.Org,
		Repo:       b.Repo,
		Branch:     b.Branch,
		Session:    b.Session,
		Name:       b.Name,
		Version:    b.Version,
		File:       b.File,
		Dockerfile: b.Dockerfile,
	})
	writeJSON(w, http.StatusAccepted, map[string]interface{}{"ok": true, "build_id": id})
}
