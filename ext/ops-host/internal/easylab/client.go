// Package easylab is the HTTP client for the easylab /api/v1/ops surface.
// ops-host is a thin tool/agent shim: every sandbox/deploy/build operation goes
// through easylab, which owns access to podman/buildah.
package easylab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client talks to one easylab base URL with one token.
type Client struct {
	base  string
	token string
	http  *http.Client
	long  *http.Client // streaming bodies (sync)
}

// New builds a client. base is the easylab root (e.g. http://127.0.0.1:18160),
// token is a write-level easylab token (sent as `Authorization: Bearer <t>`).
func New(base, token string) *Client {
	return &Client{
		base:  strings.TrimRight(base, "/"),
		token: token,
		http:  &http.Client{Timeout: 60 * time.Second},
		long:  &http.Client{Timeout: 15 * time.Minute},
	}
}

func (c *Client) addAuth(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}

func (c *Client) do(ctx context.Context, client *http.Client, method, path string, body any, contentType string) ([]byte, int, error) {
	u := c.base + path
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		rdr = bytes.NewReader(b)
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, 0, err
	}
	if contentType != "" && body == nil && contentType != "application/json" {
		req.Header.Set("Content-Type", contentType)
	}
	c.addAuth(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if resp.StatusCode >= 400 {
		return data, resp.StatusCode, fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, resp.StatusCode, nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	data, _, err := c.do(ctx, c.http, http.MethodGet, path, nil, "")
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) post(ctx context.Context, path string, body, out any) error {
	data, _, err := c.do(ctx, c.http, http.MethodPost, path, body, "")
	if err != nil {
		return err
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) delete(ctx context.Context, path string) error {
	_, _, err := c.do(ctx, c.http, http.MethodDelete, path, nil, "")
	return err
}

// ── services (sandbox containers + deployments) ──

// PortSpec is one container→service port mapping.
type PortSpec struct {
	Container int `json:"container"`
	Service   int `json:"service"`
}

// ServiceRequest is the POST /api/v1/ops/services body (easylab podman).
type ServiceRequest struct {
	Name        string            `json:"name"`
	Image       string            `json:"image"`
	Kind        string            `json:"kind"` // "bare" | "deployment"
	Command     string            `json:"command,omitempty"`
	Ports       []PortSpec        `json:"ports,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Replicas    *int              `json:"replicas,omitempty"`
	Group       string            `json:"group,omitempty"`
	Network     string            `json:"network,omitempty"`
	Namespace   string            `json:"namespace,omitempty"`
	CPUs        string            `json:"cpus,omitempty"`
	MemoryBytes uint64            `json:"memory_bytes,omitempty"`
	Annotations map[string]string `json:"labels,omitempty"`
	Resources   *ResourceSpec     `json:"resources,omitempty"`
}

// ResourceSpec is cpu/memory requests (easylab applies them via cgroups).
type ResourceSpec struct {
	CPU    string `json:"cpus,omitempty"`
	Memory string `json:"memory,omitempty"`
}

// ServiceStatus is the GET /api/v1/ops/services/{name} payload.
type ServiceStatus struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Replicas int    `json:"replicas"`
	Ready    int    `json:"ready"`
	Phase    string `json:"phase"`
	PodIP    string `json:"pod_ip"`
}

// ReadyOK reports whether the service has at least one ready replica.
func (s ServiceStatus) ReadyOK() bool { return s.Ready > 0 }

// WorkerURL derives the pod-direct worker base.
func (s ServiceStatus) WorkerURL(port int) string {
	if s.PodIP == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", s.PodIP, port)
}

// EnsureService creates-or-reuses a service.
func (c *Client) EnsureService(ctx context.Context, req ServiceRequest) (ServiceStatus, error) {
	var st ServiceStatus
	if err := c.post(ctx, "/api/v1/ops/services", req, &st); err != nil {
		return ServiceStatus{}, err
	}
	return st, nil
}

// Service fetches one service's status.
func (c *Client) Service(ctx context.Context, name, namespace string) (ServiceStatus, error) {
	var st ServiceStatus
	if err := c.get(ctx, servicePath(name, namespace), &st); err != nil {
		return ServiceStatus{}, err
	}
	return st, nil
}

// DeleteService removes the service.
func (c *Client) DeleteService(ctx context.Context, name, namespace string) error {
	return c.delete(ctx, servicePath(name, namespace))
}

// ListServices lists easylab-owned services in a namespace.
func (c *Client) ListServices(ctx context.Context, namespace string) ([]map[string]any, error) {
	var out []map[string]any
	if err := c.get(ctx, withNS("/api/v1/ops/services", namespace), &out); err != nil {
		return nil, err
	}
	return out, nil
}

// SyncRequest is the POST /api/v1/ops/services/{name}/sync body.
type SyncRequest struct {
	Org       string `json:"org"`
	Repo      string `json:"repo"`
	Rev       string `json:"rev"`
	Namespace string `json:"namespace,omitempty"`
}

// Sync pushes a repo snapshot into a service's worker (overlay extract).
func (c *Client) Sync(ctx context.Context, name string, req SyncRequest, force bool) (files int, err error) {
	p := subPath(name, req.Namespace, "/sync")
	if force {
		p += "?force=1"
	}
	var out struct {
		OK      bool `json:"ok"`
		Skipped bool `json:"skipped"`
		Files   int  `json:"files"`
	}
	if err := c.post(ctx, p, req, &out); err != nil {
		return 0, err
	}
	return out.Files, nil
}

// ServicePods lists the pods behind a service.
func (c *Client) ServicePods(ctx context.Context, name, namespace string) ([]map[string]any, error) {
	var out struct {
		Pods []map[string]any `json:"pods"`
	}
	if err := c.get(ctx, subPath(name, namespace, "/pods"), &out); err != nil {
		return nil, err
	}
	return out.Pods, nil
}

// ServiceEvents lists rollout debugging events for a service.
func (c *Client) ServiceEvents(ctx context.Context, name, namespace string) ([]map[string]any, error) {
	var out struct {
		Events []map[string]any `json:"events"`
	}
	if err := c.get(ctx, subPath(name, namespace, "/events"), &out); err != nil {
		return nil, err
	}
	return out.Events, nil
}

// ServiceRevisions lists deployment revisions.
func (c *Client) ServiceRevisions(ctx context.Context, name, namespace string) ([]map[string]any, error) {
	var out struct {
		Revisions []map[string]any `json:"revisions"`
	}
	if err := c.get(ctx, subPath(name, namespace, "/revisions"), &out); err != nil {
		return nil, err
	}
	return out.Revisions, nil
}

// RollbackService replays a revision (0 = previous).
func (c *Client) RollbackService(ctx context.Context, name string, revision int64, namespace string) error {
	body := map[string]any{"revision": revision}
	if namespace != "" {
		body["namespace"] = namespace
	}
	return c.post(ctx, subPath(name, namespace, "/rollback"), body, nil)
}

// RestartService triggers a service restart.
func (c *Client) RestartService(ctx context.Context, name, namespace string) error {
	body := map[string]any{}
	if namespace != "" {
		body["namespace"] = namespace
	}
	return c.post(ctx, servicePath(name, namespace), body, nil)
}

// ScaleService sets a service's replica count.
func (c *Client) ScaleService(ctx context.Context, name string, replicas int, namespace string) error {
	body := map[string]any{"replicas": replicas}
	if namespace != "" {
		body["namespace"] = namespace
	}
	return c.post(ctx, subPath(name, namespace, "/scale"), body, nil)
}

func servicePath(name, namespace string) string {
	p := "/api/v1/ops/services/" + url.PathEscape(name)
	if namespace != "" {
		p += "?namespace=" + url.QueryEscape(namespace)
	}
	return p
}

func subPath(name, namespace, sub string) string {
	p := "/api/v1/ops/services/" + url.PathEscape(name) + sub
	if namespace != "" {
		p += "?namespace=" + url.QueryEscape(namespace)
	}
	return p
}

func withNS(path, namespace string) string {
	if namespace == "" {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + "namespace=" + url.QueryEscape(namespace)
}

// ── one-shot runs ──

// RunRequest is the POST /api/v1/ops/runs body.
type RunRequest struct {
	Image      string            `json:"image"`
	Command    string            `json:"command,omitempty"`
	Env        map[string]string `json:"env,omitempty"`
	CPU        string            `json:"cpu,omitempty"`
	Memory     string            `json:"memory,omitempty"`
	TimeoutSec int               `json:"timeout_secs,omitempty"`
	Namespace  string            `json:"namespace,omitempty"`
}

// Run starts a one-shot run and returns the task id.
func (c *Client) Run(ctx context.Context, req RunRequest) (string, error) {
	var out struct {
		RunID string `json:"run_id"`
	}
	if err := c.post(ctx, "/api/v1/ops/runs", req, &out); err != nil {
		return "", err
	}
	return out.RunID, nil
}

// ── tasks ──

// Task is a polled ops task (build/run).
type Task struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Result string `json:"result"`
	Error  string `json:"error"`
}

// Task fetches a task's current state.
func (c *Client) Task(ctx context.Context, id string) (Task, error) {
	var t Task
	if err := c.get(ctx, "/api/v1/ops/tasks/"+url.PathEscape(id), &t); err != nil {
		return Task{}, err
	}
	return t, nil
}

// ── namespaces ──

// RegisterNamespace approves a runtime namespace for /ops operations.
func (c *Client) RegisterNamespace(ctx context.Context, namespace string) error {
	return c.post(ctx, "/api/v1/ops/namespaces", map[string]any{"namespace": namespace}, nil)
}

// ── config ──

// Config is the GET /api/v1/ops/config payload.
type Config struct {
	Namespaces       []string `json:"namespaces"`
	DefaultNamespace string   `json:"default_namespace"`
}

// Config fetches the ops surface capabilities.
func (c *Client) Config(ctx context.Context) (Config, error) {
	var cfg Config
	if err := c.get(ctx, "/api/v1/ops/config", &cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
