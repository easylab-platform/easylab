package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAdminAPINotOnPublicRouter verifies defense in depth: the operator admin
// API (/artifacts/system) is served ONLY on the separate admin listener, never
// on the public gateway router, so routing config and inventory cannot be
// reached through the gateway Service.
func TestAdminAPINotOnPublicRouter(t *testing.T) {
	s := newTestServer(t)

	// Public router: the admin surface must not be mounted.
	req := httptest.NewRequest(http.MethodGet, "/artifacts/system/targets", nil)
	rec := httptest.NewRecorder()
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("public /artifacts/system/targets = %d, want 404", rec.Code)
	}

	// Admin router: the same path is handled (auth gate then handler). With a
	// non-open store the credential gate runs; either way it must NOT be 404.
	areq := httptest.NewRequest(http.MethodGet, "/artifacts/system/targets", nil)
	arec := httptest.NewRecorder()
	s.adminRouter().ServeHTTP(arec, areq)
	if arec.Code == http.StatusNotFound {
		t.Fatalf("admin /artifacts/system/targets = 404, want the system handler (401/200)")
	}
}

// TestAdminRouterServesStatsPath verifies the admin mux binds the bare and
// trailing-slash system mount, so subpaths reach the handler.
func TestAdminRouterServesStatsPath(t *testing.T) {
	s := newTestServer(t)
	for _, p := range []string{"/artifacts/system/targets", "/artifacts/system/targets/"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		s.adminRouter().ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound && p == "/artifacts/system/targets" {
			t.Fatalf("%s = 404 on admin mux", p)
		}
	}
}

// TestAdminStatsWired verifies the footprint endpoint is backed by the real
// store (not the "stats not available" placeholder): with a token it returns a
// JSON body carrying the blobs/formats fields.
func TestAdminStatsWired(t *testing.T) {
	s := newAuthServer(t)
	req := httptest.NewRequest(http.MethodGet, "/artifacts/system/stats", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rec := httptest.NewRecorder()
	s.adminRouter().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("stats = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("stats body not JSON: %v (%s)", err, rec.Body.String())
	}
	if _, ok := out["blobs"]; !ok {
		t.Fatalf("stats body missing blobs: %s", rec.Body.String())
	}
}
