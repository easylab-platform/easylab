package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestLabUnifiedWriteAmend verifies the unified /files write: default amends
// onto the branch tip (revision_id stable), new_commit creates a fresh rev,
// and a missing default branch on first write creates a root commit.
func TestLabUnifiedWriteAmend(t *testing.T) {
	s := newLabServer(t)
	admin := &labClient{t: t, s: s, token: "lab-admin"}
	seedTestToken(t, s, "lab-admin")
	admin.ok("POST", "/api/v1/repo", map[string]any{"namespace": "team", "name": "app"})

	// First write: no branch exists yet -> creates a root commit on "main".
	r1 := admin.ok("POST", "/api/v1/repo/team/app/files", map[string]any{
		"changes": []map[string]any{{"path": "a.txt", "content": "a\n"}},
	})
	rev1 := r1["revision_id"].(string)
	if rev1 == "" {
		t.Fatal("first write should produce a revision_id")
	}
	if r1["amended"] != false {
		t.Fatalf("first (root) write should not be amended: %v", r1)
	}

	// Second write (no new_commit): should AMEND onto rev1 -> same revision_id.
	r2 := admin.ok("POST", "/api/v1/repo/team/app/files", map[string]any{
		"changes": []map[string]any{{"path": "a.txt", "content": "a2\n"}},
	})
	if r2["revision_id"] != rev1 {
		t.Fatalf("default write should amend (same id): %s != %s", r2["revision_id"], rev1)
	}
	if r2["amended"] != true {
		t.Fatalf("default write should be amended=true: %v", r2)
	}

	// Third write with new_commit=true -> fresh revision.
	r3 := admin.ok("POST", "/api/v1/repo/team/app/files", map[string]any{
		"changes":    []map[string]any{{"path": "b.txt", "content": "b\n"}},
		"new_commit": true,
	})
	if r3["revision_id"] == rev1 {
		t.Fatalf("new_commit should create a fresh revision: %s", r3["revision_id"])
	}

	// Verify the multi-file changes land together on the latest revision.
	var revs []map[string]any
	rr := admin.do("GET", "/api/v1/repo/team/app/revisions", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("revisions: %d", rr.Code)
	}
	_ = json.Unmarshal(rr.Body.Bytes(), &revs)
	if len(revs) != 2 {
		t.Fatalf("expected 2 revisions (root-amended + new), got %d", len(revs))
	}
}

// TestLabContentsRead verifies GET /contents/{path} returns base64 + sha + size.
func TestLabContentsRead(t *testing.T) {
	s := newLabServer(t)
	admin := &labClient{t: t, s: s, token: "lab-admin"}
	seedTestToken(t, s, "lab-admin")
	admin.ok("POST", "/api/v1/repo", map[string]any{"namespace": "team", "name": "app"})
	admin.ok("POST", "/api/v1/repo/team/app/files", map[string]any{
		"changes": []map[string]any{{"path": "dir/hello.txt", "content": "hello\n"}},
	})
	resp := admin.do("GET", "/api/v1/repo/team/app/contents/dir/hello.txt", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("contents: %d %s", resp.Code, resp.Body.String())
	}
	var v map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v["sha"] == "" || v["size"] == nil {
		t.Fatalf("contents missing sha/size: %v", v)
	}
	// Decode base64 content.
	content, _ := v["content"].(string)
	if content == "" {
		t.Fatalf("contents missing base64 content: %v", v)
	}
	// "hello\n" base64
	if !containsStr(content, "aGVsbG8") {
		t.Fatalf("content not base64 hello: %q", content)
	}
}
