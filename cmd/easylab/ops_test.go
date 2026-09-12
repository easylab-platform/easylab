package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpsBuildEndToEnd exercises POST /api/v1/ops/builds through the router.
// The k8s backend is only wired in-cluster; off-cluster the endpoint reports
// 501 (not implemented) rather than silently succeeding. The live build path
// is exercised in-cluster.
func TestOpsBuildEndToEnd(t *testing.T) {
	s := newTestServer(t)
	seedTestToken(t, s, "t")

	body := strings.NewReader(`{"context":"/data/e2e-bctx","image":"e2e-hello:1"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/ops/builds", body)
	req.Header.Set("Authorization", "Bearer t")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("build without k8s: expected 501, got %d %s", rec.Code, rec.Body.String())
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
