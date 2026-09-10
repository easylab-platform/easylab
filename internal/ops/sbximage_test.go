package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFetchWorkerBin(t *testing.T) {
	payload := []byte("fake-easyworker-binary")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pkgs/generic/easyworker/v0.2.0/easyworker-linux-amd64" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer, got %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	t.Setenv("EASYLAB_ARTIFACT_URL", srv.URL)
	t.Setenv("ARTIFACT_TOKEN", "tok")
	t.Setenv("EASYLAB_WORKER_BIN", "")
	t.Setenv("EASYLAB_WORKER_REF", "")

	got, err := fetchWorkerBin(context.Background())
	if err != nil {
		t.Fatalf("fetchWorkerBin: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload mismatch: %q", got)
	}
}

func TestLoadWorkerBinPrefersLocalFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "easyworker")
	want := []byte("local-binary")
	if err := os.WriteFile(p, want, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EASYLAB_WORKER_BIN", p)
	t.Setenv("EASYLAB_ARTIFACT_URL", "http://127.0.0.1:1") // must not be contacted

	got, err := loadWorkerBin(context.Background())
	if err != nil {
		t.Fatalf("loadWorkerBin: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestWorkerRef(t *testing.T) {
	t.Setenv("EASYLAB_WORKER_REF", "")
	if n, v := WorkerRef(); n != "easyworker" || v != "v0.2.0" {
		t.Fatalf("default ref = %s@%s", n, v)
	}
	t.Setenv("EASYLAB_WORKER_REF", "myworker@v2.3.4")
	if n, v := WorkerRef(); n != "myworker" || v != "v2.3.4" {
		t.Fatalf("ref = %s@%s", n, v)
	}
	t.Setenv("EASYLAB_WORKER_REF", "solo")
	if n, v := WorkerRef(); n != "solo" || v != "v0.2.0" {
		t.Fatalf("ref = %s@%s", n, v)
	}
}

func TestSandboxTagStable(t *testing.T) {
	bin := []byte("abc")
	h := sha256.New()
	h.Write([]byte("img"))
	h.Write([]byte{'|'})
	h.Write(bin)
	want := "sbx.internal/easyworker:" + hex.EncodeToString(h.Sum(nil))[:16]
	if got := SandboxTag("img", bin); got != want {
		t.Fatalf("SandboxTag = %s want %s", got, want)
	}
}
