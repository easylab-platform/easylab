package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"easyvcs-ext-ops/internal/easylab"
)

// --- flat tarball fixture (easylab archive has no top-level dir) --------

func buildFlatArchive(files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))}
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write([]byte(content))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func readArchive(data []byte) []string {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var out []string
	for {
		hdr, err := tr.Next()
		if err != nil {
			return out
		}
		out = append(out, hdr.Name)
	}
}

// --- branch resolution ---------------------------------------------

func newFakeLab(t *testing.T, branchs string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/verify/exists/branchs", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(branchs))
	})
	mux.HandleFunc("/api/v1/repos/verify/nope/branchs", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"branchs":[]}`))
	})
	mux.HandleFunc("/api/v1/repos/verify/ws/", func(w http.ResponseWriter, r *http.Request) {
		// easylab tarball is flat: /api/v1/repos/verify/ws/archive/tarball/{rev}
		rev := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/verify/ws/archive/tarball/")
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(buildFlatArchive(map[string]string{
			"file.txt": "content-" + rev,
		}))
	})
	return httptest.NewServer(mux)
}

func TestBranchHead(t *testing.T) {
	lab := newFakeLab(t, `{"branchs":[{"name":"main","sha":"abc123"}]}`)
	defer lab.Close()
	s := &server{base: lab.URL, wsCache: map[string]wsCacheEntry{}}

	rev, err := s.easylabBranchHead(context.Background(), "verify", "exists", "main")
	if err != nil || rev != "abc123" {
		t.Fatalf("rev=%q err=%v", rev, err)
	}
	if _, err := s.easylabBranchHead(context.Background(), "verify", "nope", "main"); err == nil {
		t.Fatal("missing branch should error")
	}
}

func TestResolveWorkspaceSessionName(t *testing.T) {
	lab := newFakeLab(t, `{"branchs":[{"name":"main","sha":"abc123"}]}`)
	defer lab.Close()
	s := &server{base: lab.URL, wsCache: map[string]wsCacheEntry{}}

	ws, sid, err := s.resolveWorkspace(context.Background(), map[string]interface{}{}, "verify:exists:main")
	if err != nil || ws.org != "verify" || ws.repo != "exists" || ws.branch != "main" || ws.rev != "abc123" || sid != "verify:exists:main" {
		t.Fatalf("ws=%+v sid=%q err=%v", ws, sid, err)
	}

	if _, _, err := s.resolveWorkspace(context.Background(), map[string]interface{}{}, "not-a-derived-name"); err == nil {
		t.Fatal("non-derived session name must error")
	}
}

func TestSyncStateMachine(t *testing.T) {
	var syncs int32
	var lastBody []byte
	var lastPath string
	fakeLab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/sync") {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&syncs, 1)
		lastPath = r.URL.Path
		lastBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"ok":true,"skipped":false,"files":1}`))
	}))
	defer fakeLab.Close()

	s := &server{ops: easylab.New(fakeLab.URL, "devtoken"), runtimeNamespace: "temp",
		wsCache: map[string]wsCacheEntry{}, synced: map[string]string{}}
	ws := workspace{org: "verify", repo: "ws", branch: "main", rev: "rev1"}

	// First ensure → syncs.
	if err := s.ensureSynced(context.Background(), "cid1", "verify:ws:main", ws); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&syncs); n != 1 {
		t.Fatalf("syncs=%d want 1", n)
	}
	// The sync request must name the org/repo/rev.
	var reqBody struct {
		Org  string `json:"org"`
		Repo string `json:"repo"`
		Rev  string `json:"rev"`
	}
	_ = json.Unmarshal(lastBody, &reqBody)
	if reqBody.Org != "verify" || reqBody.Repo != "ws" || reqBody.Rev != "rev1" {
		t.Fatalf("sync body = %+v", reqBody)
	}
	// The path must address the session's sandbox (labelKey of the session).
	wantPath := "/api/v1/ops/services/" + labelKey("verify:ws:main") + "/sync"
	if lastPath != wantPath {
		t.Fatalf("sync path = %q want %q", lastPath, wantPath)
	}

	// Second ensure with same rev → cached, no extra sync.
	if err := s.ensureSynced(context.Background(), "cid1", "verify:ws:main", ws); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&syncs); n != 1 {
		t.Fatalf("syncs=%d want 1 (cached)", n)
	}

	// New rev → syncs again.
	ws.rev = "rev2"
	if err := s.ensureSynced(context.Background(), "cid1", "verify:ws:main", ws); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&syncs); n != 2 {
		t.Fatalf("syncs=%d want 2", n)
	}

	// Worker restart (need_sync): markUnsynced forces a re-push.
	s.markUnsynced("cid1")
	if err := s.ensureSynced(context.Background(), "cid1", "verify:ws:main", ws); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&syncs); n != 3 {
		t.Fatalf("syncs=%d want 3", n)
	}

	// easylab failure surfaces as an error (unreachable server).
	closed := easylab.New("http://127.0.0.1:1", "")
	if _, err := closed.Sync(context.Background(), "x", easylab.SyncRequest{
		Org: "o", Repo: "r", Rev: "v"}, false); err == nil {
		t.Fatal("unreachable easylab must error")
	}
}

func TestSyncWorkerRejectsBadResponse(t *testing.T) {
	fakeLab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"ok":false,"error":"worker sync 500: boom"}`))
	}))
	defer fakeLab.Close()
	s := &server{ops: easylab.New(fakeLab.URL, "devtoken"), runtimeNamespace: "temp",
		synced: map[string]string{}}
	ws := workspace{org: "o", repo: "r", branch: "main", rev: "v"}
	if err := s.ensureSynced(context.Background(), "cid", "o:r:main", ws); err == nil {
		t.Fatal("easylab error must propagate")
	}
	if s.syncedRev("cid") != "" {
		t.Fatal("rev must not be recorded on failure")
	}
}
