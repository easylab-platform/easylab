package main

import (
	"context"
	"encoding/json"
	"net/http"

	"easyvcs-ext-ops/internal/easylab"
	"strings"
)

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": "ops-extension"})
}

func (s *server) k8sConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok":           true,
		"namespace":    s.runtimeNamespace,
		"worker_image": s.workerImage,
	})
}

// sessionParam returns the deterministic container key for the {session} path
// parameter (raw "org:repo:branch" → hash).
func sessionParam(r *http.Request) string {
	return sessionKey(param(r, "session"))
}

func (s *server) deleteContainer(w http.ResponseWriter, r *http.Request) {
	session := param(r, "session")
	if err := s.destroyWorker(r.Context(), session); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true})
}

func (s *server) exec(w http.ResponseWriter, r *http.Request) {
	key := sessionParam(r)
	var b struct {
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil || b.Command == "" {
		writeErr(w, http.StatusBadRequest, "command required")
		return
	}
	res, err := s.workerCommand(r.Context(), key, "execute", map[string]interface{}{"command": b.Command})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) listJobs(w http.ResponseWriter, r *http.Request) {
	key := sessionParam(r)
	res, err := s.workerCommand(r.Context(), key, "jobs", map[string]interface{}{})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"jobs": res})
}

func (s *server) jobOutput(w http.ResponseWriter, r *http.Request) {
	key := sessionParam(r)
	jobID := param(r, "jobID")
	q := r.URL.Query()
	res, err := s.workerCommand(r.Context(), key, "job_output", map[string]interface{}{
		"job-id": jobID,
		"stream": q.Get("stream"),
		"start":  parseIntOr(q.Get("start"), -200),
		"end":    parseIntOr(q.Get("end"), -1),
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) jobWait(w http.ResponseWriter, r *http.Request) {
	key := sessionParam(r)
	jobID := param(r, "jobID")
	var b struct {
		TimeoutMS *int64 `json:"timeout-ms"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	timeout := int64(30000)
	if b.TimeoutMS != nil {
		timeout = *b.TimeoutMS
	}
	// Clamp to [1s, 60s]: the worker caps job_wait at 60s and the WS read
	// deadline is 65s, so anything above 60s can never return in time.
	if timeout < 1000 {
		timeout = 1000
	}
	if timeout > 60_000 {
		timeout = 60_000
	}
	res, err := s.workerCommand(r.Context(), key, "job_wait", map[string]interface{}{
		"job-id":     jobID,
		"timeout-ms": timeout,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) jobStdin(w http.ResponseWriter, r *http.Request) {
	key := sessionParam(r)
	jobID := param(r, "jobID")
	var b struct {
		Data  string `json:"data"`
		Close bool   `json:"close"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	res, err := s.workerCommand(r.Context(), key, "job_stdin", map[string]interface{}{
		"job-id": jobID,
		"data":   b.Data,
		"close":  b.Close,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) kill(w http.ResponseWriter, r *http.Request) {
	key := sessionParam(r)
	jobID := param(r, "jobID")
	res, err := s.workerCommand(r.Context(), key, "kill", map[string]interface{}{"job-id": jobID})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "result": res})
}

func (s *server) sandboxRead(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ContainerID string `json:"container_id"`
		Path        string `json:"path"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	cid := b.ContainerID
	if cid == "" {
		cid = sessionParam(r)
	}
	data, err := s.sandboxFileRead(r.Context(), cid, b.Path)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "content": string(data)})
}

func (s *server) sandboxWrite(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ContainerID string `json:"container_id"`
		Path        string `json:"path"`
		Content     string `json:"content"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	cid := b.ContainerID
	if cid == "" {
		cid = sessionParam(r)
	}
	if err := s.sandboxFileWrite(r.Context(), cid, b.Path, []byte(b.Content)); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "path": b.Path})
}

func (s *server) deploy(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name      string            `json:"name"`
		Image     string            `json:"image"`
		Replicas  int32             `json:"replicas"`
		Port      int32             `json:"port"`
		Env       map[string]string `json:"env"`
		Session   string            `json:"session"`
		Resources *ResourceRequest  `json:"resources"`
	}
	_ = json.NewDecoder(r.Body).Decode(&b)
	if b.Name == "" || b.Image == "" {
		writeErr(w, http.StatusBadRequest, "name/image required")
		return
	}
	if b.Port == 0 {
		b.Port = 8080
	}
	if _, err := s.ops.EnsureService(r.Context(), s.deploymentRequest(b)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": b.Name, "image": b.Image})
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

var _ = context.Background

// ResourcePair is one side of a nested resource declaration.
type ResourcePair struct {
	CPU    string `json:"cpu,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// ResourceRequest is the nested `resources:{requests:{...},limits:{...}}`
// shape accepted by the deploy API and the service-deploy tool. easylab
// applies CPU/memory to the workload's requests+limits.
type ResourceRequest struct {
	Requests *ResourcePair `json:"requests,omitempty"`
	Limits   *ResourcePair `json:"limits,omitempty"`
}

// deploymentRequest converts the legacy deploy body into a easylab service
// request (a user-app deployment with an http port).
func (s *server) deploymentRequest(b struct {
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	Replicas  int32             `json:"replicas"`
	Port      int32             `json:"port"`
	Env       map[string]string `json:"env"`
	Session   string            `json:"session"`
	Resources *ResourceRequest  `json:"resources"`
}) easylab.ServiceRequest {
	req := easylab.ServiceRequest{
		Name:      b.Name,
		Image:     b.Image,
		Kind:      "deployment",
		Ports:     []easylab.PortSpec{{Container: int(b.Port), Service: 80}},
		Env:       b.Env,
		Namespace: s.runtimeNamespace,
	}
	if b.Replicas > 0 {
		replicas := int(b.Replicas)
		req.Replicas = &replicas
	}
	if b.Session != "" {
		req.Annotations = map[string]string{"zergx/session": b.Session}
	}
	if b.Resources != nil && (b.Resources.Requests != nil || b.Resources.Limits != nil) {
		res := easylab.ResourceSpec{}
		if b.Resources.Requests != nil {
			res.CPU = b.Resources.Requests.CPU
			res.Memory = b.Resources.Requests.Memory
		} else if b.Resources.Limits != nil {
			res.CPU = b.Resources.Limits.CPU
			res.Memory = b.Resources.Limits.Memory
		}
		req.Resources = &res
	}
	return req
}
