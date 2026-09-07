package main

import (
	"context"
	"google.golang.org/protobuf/proto"

	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	easylabsdk "github.com/easylab-platform/easylab-client-sdk"
)

// --- fake easylab Connect server (OpsService Sync + LabService Branches) ---

// newFakeLab serves a minimal Connect surface: Sync on OpsService and Branches
// on LabService, using the Connect unary protocol (application/proto).
func newFakeLab(t *testing.T, handler func(proc string, body []byte) (any, []byte, int)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/easylab.v1.OpsService/Sync", func(w http.ResponseWriter, r *http.Request) {
		body := readAll(t, r)
		msg := &easylabv1.SyncRequest{}
		_ = proto.Unmarshal(body, msg)
		resp := &easylabv1.SyncResponse{Ok: true, Files: 1}
		out, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/proto")
		_, _ = w.Write(out)
	})
	mux.HandleFunc("/easylab.v1.LabService/Branches", func(w http.ResponseWriter, r *http.Request) {
		body := readAll(t, r)
		msg := &easylabv1.BranchesRequest{}
		_ = proto.Unmarshal(body, msg)
		if strings.Contains(msg.GetRepo(), "nope") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		resp := &easylabv1.BranchesResponse{Branches: []*easylabv1.BranchInfo{{Name: "main", Sha: "abc123"}}}
		out, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/proto")
		_, _ = w.Write(out)
	})
	return httptest.NewServer(mux)
}

func readAll(t *testing.T, r *http.Request) []byte {
	t.Helper()
	buf := make([]byte, r.ContentLength)
	_, _ = r.Body.Read(buf)
	return buf
}

func TestBranchHead(t *testing.T) {
	lab := newFakeLab(t, nil)
	defer lab.Close()
	s := &server{sdk: easylabsdk.New(lab.URL, "devtoken"), wsCache: map[string]wsCacheEntry{}}

	rev, err := s.easylabBranchHead(context.Background(), "verify", "exists", "main")
	if err != nil || rev != "abc123" {
		t.Fatalf("rev=%q err=%v", rev, err)
	}
	if _, err := s.easylabBranchHead(context.Background(), "verify", "nope", "missing"); err == nil {
		t.Fatal("missing branch should error")
	}
}

func TestResolveWorkspaceSessionName(t *testing.T) {
	lab := newFakeLab(t, nil)
	defer lab.Close()
	s := &server{sdk: easylabsdk.New(lab.URL, "devtoken"), wsCache: map[string]wsCacheEntry{}}

	ws, _, err := s.resolveWorkspace(context.Background(), nil, "verify:exists:main")
	if err != nil {
		t.Fatal(err)
	}
	if ws.org != "verify" || ws.repo != "exists" || ws.branch != "main" || ws.rev != "abc123" {
		t.Fatalf("ws=%+v", ws)
	}
	// Cached resolution (no further calls needed).
	ws2, _, err := s.resolveWorkspace(context.Background(), nil, "verify:exists:main")
	if err != nil || ws2.rev != "abc123" {
		t.Fatalf("cached ws=%+v err=%v", ws2, err)
	}
}

func TestSyncStateMachine(t *testing.T) {
	var syncs int32
	mux := http.NewServeMux()
	mux.HandleFunc("/easylab.v1.OpsService/Sync", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&syncs, 1)
		resp := &easylabv1.SyncResponse{Ok: true, Files: 1}
		out, _ := proto.Marshal(resp)
		w.Header().Set("Content-Type", "application/proto")
		_, _ = w.Write(out)
	})
	fakeLab := httptest.NewServer(mux)
	defer fakeLab.Close()

	s := &server{sdk: easylabsdk.New(fakeLab.URL, "devtoken"), runtimeNamespace: "temp",
		wsCache: map[string]wsCacheEntry{}, synced: map[string]string{}}
	ws := workspace{org: "verify", repo: "ws", branch: "main", rev: "rev1"}

	if err := s.ensureSynced(context.Background(), "cid1", "verify:ws:main", ws); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&syncs); n != 1 {
		t.Fatalf("syncs=%d want 1", n)
	}
	// Cached rev → no extra sync.
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
	// markUnsynced forces a re-push.
	s.markUnsynced("cid1")
	if err := s.ensureSynced(context.Background(), "cid1", "verify:ws:main", ws); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&syncs); n != 3 {
		t.Fatalf("syncs=%d want 3", n)
	}
	// Unreachable easylab surfaces as an error.
	closed := easylabsdk.New("http://127.0.0.1:1", "")
	if _, err := closed.Sync(context.Background(), "x", "o", "r", "v", "", false); err == nil {
		t.Fatal("unreachable easylab must error")
	}
}

func TestSyncWorkerRejectsBadResponse(t *testing.T) {
	fakeLab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer fakeLab.Close()
	s := &server{sdk: easylabsdk.New(fakeLab.URL, "devtoken"), runtimeNamespace: "temp",
		synced: map[string]string{}}
	ws := workspace{org: "o", repo: "r", branch: "main", rev: "v"}
	if err := s.ensureSynced(context.Background(), "cid", "o:r:main", ws); err == nil {
		t.Fatal("easylab error must propagate")
	}
	if s.syncedRev("cid") != "" {
		t.Fatal("rev must not be recorded on failure")
	}
}

var _ = connect.CodeInternal
