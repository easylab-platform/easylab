package main

import (
	"context"
	"io"

	"google.golang.org/protobuf/proto"

	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	easylabsdk "github.com/easylab-platform/easylab-sdk-go"
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
	// easylab SDK speaks HTTP/2 only; make the fake gateway dual-stack.
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv := httptest.NewUnstartedServer(mux)
	srv.Config.Protocols = protocols
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
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

func TestEnsureSyncedDelegatesToEasylab(t *testing.T) {
	var got *easylabv1.SyncWorkspaceRequest
	mux := http.NewServeMux()
	mux.HandleFunc("/easylab.v1.SandboxService/SyncWorkspace", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req := &easylabv1.SyncWorkspaceRequest{}
		_ = proto.Unmarshal(body, req)
		got = req
		out, _ := proto.Marshal(&easylabv1.SyncWorkspaceResponse{SyncedRev: "tree1"})
		w.Header().Set("Content-Type", "application/proto")
		_, _ = w.Write(out)
	})
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	fakeLab := httptest.NewUnstartedServer(mux)
	fakeLab.Config.Protocols = protocols
	fakeLab.Start()
	defer fakeLab.Close()

	s := &server{sdk: easylabsdk.New(fakeLab.URL, "devtoken"), wsCache: map[string]wsCacheEntry{}}
	ws := workspace{org: "verify", repo: "ws", branch: "main", rev: "rev1"}
	if err := s.ensureSynced(context.Background(), "cid1", "verify:ws:main", ws); err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("SyncWorkspace was not called")
	}
	if got.Sandbox != "cid1" || got.Org != "verify" || got.Repo != "ws" || got.Rev != "rev1" || got.Branch != "main" {
		t.Fatalf("sync req = %+v", got)
	}
}

func TestEnsureSyncedPropagatesErrors(t *testing.T) {
	fakeLab := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer fakeLab.Close()
	s := &server{sdk: easylabsdk.New(fakeLab.URL, "devtoken")}
	ws := workspace{org: "o", repo: "r", branch: "main", rev: "v"}
	if err := s.ensureSynced(context.Background(), "cid", "o:r:main", ws); err == nil {
		t.Fatal("easylab error must propagate")
	}
}

var _ = connect.CodeInternal
