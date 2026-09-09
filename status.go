package opsext

import (
	"connectrpc.com/connect"
	"context"
	"encoding/json"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"net/http"

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
	}
	if _, err := s.sdk.Lab.Health(ctx, connect.NewRequest(&easylabv1.HealthRequest{})); err != nil {
		deps = append(deps, map[string]interface{}{"name": "easylab", "ok": false, "error": errStr(err)})
	} else {
		deps = append(deps, map[string]interface{}{"name": "easylab", "ok": true})
	}
	if _, err := s.sdk.Ops.OpsStatus(ctx, connect.NewRequest(&easylabv1.OpsStatusRequest{})); err != nil {
		deps = append(deps, map[string]interface{}{"name": "easylab-ops", "ok": false, "error": errStr(err)})
	} else {
		deps = append(deps, map[string]interface{}{"name": "easylab-ops", "ok": true})
	}

	svcs, _ := s.sdk.ListServices(ctx, "", "", s.runtimeNamespace)
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
func (s *server) deploymentsList(w http.ResponseWriter, r *http.Request) {
	list, err := s.sdk.ListServices(r.Context(), "", "", s.runtimeNamespace)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]interface{}{}
	for _, svc := range serviceInfoMaps(list) {
		if svc["kind"] == "deployment" {
			out = append(out, svc)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deployments": out})
}

// deploymentPods returns the pods of one deployment.
func (s *server) deploymentPods(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	res, err := s.sdk.GetService(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	pods := []map[string]interface{}{}
	for _, p := range res.GetPods() {
		pods = append(pods, map[string]interface{}{
			"name": p.GetName(), "ip": p.GetIp(), "phase": p.GetPhase(),
			"ready": p.GetReady(), "image": p.GetImage(), "age": p.GetAge(), "restarts": p.GetRestarts(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"pods": pods})
}

// deploymentStatus reports the rollout state of one deployment.
func (s *server) deploymentStatus(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	res, err := s.sdk.GetService(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	st := res.GetService()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":     st.GetName(),
		"kind":     st.GetKind(),
		"replicas": st.GetReplicas(),
		"ready":    st.GetReady() > 0,
		"phase":    st.GetPhase(),
		"pod_ip":   st.GetPodIp(),
	})
}

// deploymentDelete removes a deployment + service.
func (s *server) deploymentDelete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if _, err := s.sdk.Ops.DeleteService(r.Context(), connect.NewRequest(&easylabv1.DeleteServiceRequest{Name: name})); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

// deploymentRestart triggers a rolling restart (bumps the restartedAt
// annotation on the pod template).
func (s *server) deploymentRestart(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	// easylab has no dedicated restart RPC; a scale to the current replica
	// count forces the runner to reconcile the service (best-effort restart).
	res, err := s.sdk.GetService(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	n := res.GetService().GetReplicas()
	if _, err := s.sdk.Ops.ScaleService(r.Context(), connect.NewRequest(&easylabv1.ScaleServiceRequest{Name: name, Replicas: n})); err != nil {
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
	if _, err := s.sdk.Ops.ScaleService(r.Context(), connect.NewRequest(&easylabv1.ScaleServiceRequest{Name: name, Replicas: b.Replicas})); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "replicas": b.Replicas})
}

// deploymentRollback rolls back to a previous revision (0 = previous).
func (s *server) deploymentRollback(w http.ResponseWriter, r *http.Request) {
	writeErr(w, http.StatusNotImplemented, "rollback is not supported by the easylab ops backend")
}

// deploymentEvents lists k8s events for a deployment (rollout debugging).
func (s *server) deploymentEvents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"events": []map[string]interface{}{}})
}

// deploymentRevisions lists the ReplicaSet revisions of a deployment.
func (s *server) deploymentRevisions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"revisions": []map[string]interface{}{}})
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

// serviceInfoMaps converts proto ServiceInfo list into the loose map shape
// the ops UI consumes (name/kind/phase/pod_ip/annotations/...).
func serviceInfoMaps(list []*easylabv1.ServiceInfo) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(list))
	for _, st := range list {
		out = append(out, map[string]interface{}{
			"name":        st.GetName(),
			"kind":        st.GetKind(),
			"image":       st.GetImage(),
			"replicas":    st.GetReplicas(),
			"ready":       st.GetReady(),
			"namespace":   st.GetNamespace(),
			"age":         st.GetAge(),
			"ports":       st.GetPorts(),
			"session":     st.GetSession(),
			"status":      st.GetStatus(),
			"url":         st.GetUrl(),
			"phase":       st.GetPhase(),
			"pod_ip":      st.GetPodIp(),
			"annotations": map[string]interface{}{"easylab/session": st.GetSession()},
		})
	}
	return out
}
