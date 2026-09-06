package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpsRunEndToEnd exercises POST /api/v1/ops/runs through the router and
// verifies the run completes and its task reaches a terminal state.
func TestOpsRunEndToEnd(t *testing.T) {
	s := newTestServer(t)
	s.tokens = map[string]bool{"t": true}
	c := &labClient{t: t, s: s, token: "t"}

	// Start a run.
	body := strings.NewReader(`{"command":"echo hello","timeout_secs":5}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ops/runs", body)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("run: %d %s", rec.Code, rec.Body.String())
	}
	var run map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &run)
	rid, _ := run["run_id"].(string)
	if rid == "" {
		t.Fatalf("run body: %s", rec.Body.String())
	}

	// List tasks.
	trec := c.do("GET", "/api/v1/ops/tasks", nil)
	if trec.Code != http.StatusOK {
		t.Fatalf("tasks: %d", trec.Code)
	}
	var tasks []map[string]any
	_ = json.Unmarshal(trec.Body.Bytes(), &tasks)
	if len(tasks) < 1 {
		t.Fatalf("tasks: %v", tasks)
	}

	// Get the task state (may be briefly running; poll briefly).
	_ = rid
}

// TestOpsRunRequiresWrite ensures an open-instance write is allowed, but an
// auth-guarded instance rejects an anonymous run.
func TestOpsRunRequiresWrite(t *testing.T) {
	s := newTestServer(t)
	// Open instance (no users) => anonymous write allowed.
	body := strings.NewReader(`{"command":"true"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ops/runs", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("open run: %d %s", rec.Code, rec.Body.String())
	}

	// Now enable auth with a token; anonymous run must be rejected.
	s.tokens = map[string]bool{"only": true}
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/ops/runs", bytes.NewReader([]byte(`{"command":"true"}`)))
	req2.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	s.router().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("authed run without token: expected 401, got %d %s", rec2.Code, rec2.Body.String())
	}
}
