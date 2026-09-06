package webapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"easyvcs/internal/store"
)

func newTestAPI(t *testing.T) *API {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	return &API{CS: cs}
}

// newFakeBackend returns an httptest server that fakes the easylab /api/v1 and
// /v2 surface (services + catalog + releases) so aggregate handlers can be
// checked without a real running easylab.
func newFakeBackend(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/ops/services", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"name":"web","kind":"deployment","replicas":2},
			{"name":"db","kind":"container","replicas":1}
		]`))
	})
	mux.HandleFunc("/v2/_catalog", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"repositories":["library/busybox","acme/app"]}`))
	})
	mux.HandleFunc("/api/v1/repo/acme/app/releases", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"tag":"v1.0.0"},{"tag":"v1.1.0"}]`))
	})
	return httptest.NewServer(mux)
}

func TestStatus(t *testing.T) {
	a := newTestAPI(t)
	rec := httptest.NewRecorder()
	a.muxForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/status", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestReposEmpty(t *testing.T) {
	a := newTestAPI(t)
	rec := httptest.NewRecorder()
	a.muxForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/repos", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("repos: %d", rec.Code)
	}
}

func TestSPAIndex(t *testing.T) {
	a := newTestAPI(t)
	rec := httptest.NewRecorder()
	a.muxForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("spa index: %d", rec.Code)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("empty spa body")
	}
}

func TestFSReadMissing(t *testing.T) {
	a := newTestAPI(t)
	rec := httptest.NewRecorder()
	a.muxForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/fs/read?path=x", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("fs read missing repo: %d", rec.Code)
	}
}

func TestPackagesAggregate(t *testing.T) {
	a := newTestAPI(t)
	back := newFakeBackend(t)
	defer back.Close()
	a.SelfBase = back.URL

	// Need the acme/app repo to exist so releases aggregation has a target.
	if _, err := a.CS.Create(store.RepoRef{Namespace: "acme", Name: "app"}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	a.muxForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/packages", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("packages: %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "library/busybox") {
		t.Fatalf("missing catalog repo in packages: %s", body)
	}
	if !strings.Contains(body, "acme/app") {
		t.Fatalf("missing namespace/repo in releases: %s", body)
	}
	if !strings.Contains(body, "v1.1.0") {
		t.Fatalf("missing release tag in packages: %s", body)
	}
}

func TestContainersAggregate(t *testing.T) {
	a := newTestAPI(t)
	back := newFakeBackend(t)
	defer back.Close()
	a.SelfBase = back.URL

	rec := httptest.NewRecorder()
	a.muxForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/containers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("containers: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"web"`) || !strings.Contains(body, `"db"`) {
		t.Fatalf("containers missing services: %s", body)
	}
}

func TestDeploymentsAggregate(t *testing.T) {
	a := newTestAPI(t)
	back := newFakeBackend(t)
	defer back.Close()
	a.SelfBase = back.URL

	rec := httptest.NewRecorder()
	a.muxForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/deployments", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("deployments: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"web"`) {
		t.Fatalf("deployments missing web service: %s", body)
	}
	if strings.Contains(body, `"db"`) {
		t.Fatalf("deployments should exclude container-kind: %s", body)
	}
}

// muxForTest builds a mux with the API + SPA mounted.
func (a *API) muxForTest() *http.ServeMux {
	mux := http.NewServeMux()
	a.MountAPI(mux)
	a.MountSPA(mux)
	return mux
}
