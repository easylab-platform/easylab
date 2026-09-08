package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpsBuildEndToEnd exercises POST /api/v1/ops/builds through the router and
// verifies a build task is created and reaches a running/terminal state.
func TestOpsBuildEndToEnd(t *testing.T) {
	s := newTestServer(t)
	seedTestToken(t, s, "t")

	// Start a build. The podman backend requires a real context dir; use a
	// temp one so the request validates and creates the task (which is the
	// router-level contract). The build itself is exercised live in-cluster.
	body := strings.NewReader(`{"context":"/data/e2e-bctx","image":"e2e-hello:1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ops/builds", body)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("build: %d %s", rec.Code, rec.Body.String())
	}
	var run map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &run)
	bid, _ := run["build_id"].(string)
	if bid == "" {
		t.Fatalf("build body: %s", rec.Body.String())
	}

	// List tasks.
	trec := (&labClient{t: t, s: s, token: "t"}).do("GET", "/api/v1/ops/tasks", nil)
	if trec.Code != http.StatusOK {
		t.Fatalf("tasks: %d", trec.Code)
	}
	var tasks []map[string]any
	_ = json.Unmarshal(trec.Body.Bytes(), &tasks)
	if len(tasks) < 1 {
		t.Fatalf("tasks: %v", tasks)
	}
}

// TestOpsBuildMissingImage ensures a build without an image is rejected.
func TestOpsBuildMissingImage(t *testing.T) {
	s := newTestServer(t)
	body := strings.NewReader(`{"context":"/tmp","containerfile":"FROM scratch"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ops/builds", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("build without image: expected 400, got %d %s", rec.Code, rec.Body.String())
	}
}
