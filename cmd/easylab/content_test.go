package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// seedLabRepo creates a repo with a couple of commits and a branch pointing
// at the latest revision, returning the response as maps.
func seedLabRepo(t *testing.T, s *server, c *labClient, ns, name string) map[string]any {
	t.Helper()
	c.ok("POST", "/api/v1/repo", map[string]any{"namespace": ns, "name": name})
	c.ok("POST", "/api/v1/repo/"+ns+"/"+name+"/files", map[string]any{
		"changes":     []map[string]any{{"path": "a.txt", "content": "one\n"}},
		"description": "first",
		"new_commit":  true,
	})
	c.ok("POST", "/api/v1/repo/"+ns+"/"+name+"/files", map[string]any{
		"changes":     []map[string]any{{"path": "a.txt", "content": "two\n"}, {"path": "b.txt", "content": "bee\n"}},
		"description": "second",
		"new_commit":  true,
	})
	return c.ok("POST", "/api/v1/repo/"+ns+"/"+name+"/branches", map[string]any{"name": "main", "target": "@"})
}

func TestLabRevisionFiles(t *testing.T) {
	s := newLabServer(t)
	admin := &labClient{t: t, s: s, token: "lab-admin"}
	seedTestToken(t, s, "lab-admin")
	seedLabRepo(t, s, admin, "team", "app")

	// Find the revision that touched both a.txt and b.txt.
	rec := admin.do("GET", "/api/v1/repo/team/app/revisions", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("revisions: %d %s", rec.Code, rec.Body.String())
	}
	var revs []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &revs); err != nil {
		t.Fatalf("decode revisions: %v", err)
	}
	target := ""
	for _, ch := range revs {
		// labListRevisions does not carry changed_paths; fetch each revision to
		// find the one that touched b.txt.
		id := ch["revision_id"].(string)
		rr := admin.do("GET", "/api/v1/repo/team/app/revisions/"+id, nil)
		var got map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			continue
		}
		if cp, ok := got["changed_paths"].([]any); ok && containsPathList(cp, "b.txt") {
			target = id
			break
		}
	}
	if target == "" {
		t.Fatal("no revision touched b.txt")
	}

	rec2 := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/repo/team/app/revisions/"+target+"/files", nil)
	s.router().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("files: %d %s", rec2.Code, rec2.Body.String())
	}
	body := rec2.Body.String()
	if !strings.Contains(body, `"path":"a.txt"`) || !strings.Contains(body, `"path":"b.txt"`) {
		t.Fatalf("files missing paths: %s", body)
	}
	if !strings.Contains(body, `"status":"modified"`) && !strings.Contains(body, "added") {
		t.Fatalf("files missing status: %s", body)
	}
}

// mustList is unused; the revisions response is a bare array.
func containsPathList(ch []any, p string) bool {
	for _, x := range ch {
		if x == p {
			return true
		}
	}
	return false
}

func TestLabGraph(t *testing.T) {
	s := newLabServer(t)
	admin := &labClient{t: t, s: s, token: "lab-admin"}
	seedTestToken(t, s, "lab-admin")
	seedLabRepo(t, s, admin, "team", "app")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/repo/team/app/graph?limit=50", nil)
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("graph: %d %s", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	graph, ok := resp["graph"].([]any)
	if !ok {
		t.Fatalf("graph missing nodes: %s", rec.Body.String())
	}
	if len(graph) < 1 {
		t.Fatalf("graph should have at least 1 node: %s", rec.Body.String())
	}
	// Every node has revision_id, parents, is_head.
	node := graph[0].(map[string]any)
	if node["revision_id"] == "" || node["is_head"] == nil {
		t.Fatalf("node incomplete: %v", node)
	}
}

func TestLabArchive(t *testing.T) {
	s := newLabServer(t)
	admin := &labClient{t: t, s: s, token: "lab-admin"}
	seedTestToken(t, s, "lab-admin")
	seedLabRepo(t, s, admin, "team", "app")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/repo/team/app/archive/tarball/main", nil)
	s.router().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("archive: %d %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/gzip" {
		t.Fatalf("archive content-type: %s", ct)
	}
	if rec.Body.Len() < 20 {
		t.Fatalf("archive body too small: %d", rec.Body.Len())
	}
}
