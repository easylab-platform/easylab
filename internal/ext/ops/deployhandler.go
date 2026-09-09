package opsext

import (
	"encoding/json"
	"net/http"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	easylabsdk "github.com/easylab-platform/easylab-sdk-go"
)

func jsonDecode(r *http.Request, v interface{}) error {
	return json.NewDecoder(r.Body).Decode(v)
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
	_ = jsonDecode(r, &b)
	if b.Name == "" || b.Image == "" {
		writeErr(w, http.StatusBadRequest, "name/image required")
		return
	}
	if b.Port == 0 {
		b.Port = 8080
	}
	spec := s.deploymentRequest(b)
	if _, err := s.sdk.LaunchServiceFull(r.Context(), spec); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"ok": true, "name": b.Name, "image": b.Image})
}

func (s *server) deploymentRequest(b struct {
	Name      string            `json:"name"`
	Image     string            `json:"image"`
	Replicas  int32             `json:"replicas"`
	Port      int32             `json:"port"`
	Env       map[string]string `json:"env"`
	Session   string            `json:"session"`
	Resources *ResourceRequest  `json:"resources"`
}) easylabsdk.LaunchServiceSpec {
	req := easylabsdk.LaunchServiceSpec{
		Name:      b.Name,
		Image:     b.Image,
		Kind:      "deployment",
		Ports:     []*easylabv1.PortSpec{{Container: b.Port, Service: 80}},
		Env:       b.Env,
		Namespace: s.runtimeNamespace,
	}
	if b.Replicas > 0 {
		req.Replicas = b.Replicas
	}
	if b.Session != "" {
		req.Annotations = map[string]string{"easylab/session": b.Session}
	}
	if b.Resources != nil {
		if b.Resources.Requests != nil {
			req.CPUs = b.Resources.Requests.CPU
		} else if b.Resources.Limits != nil {
			req.CPUs = b.Resources.Limits.CPU
		}
	}
	return req
}
