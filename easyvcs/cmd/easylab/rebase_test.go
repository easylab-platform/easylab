package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestLabRebaseMany verifies the /rebase-many endpoint moves a sequence of
// revisions onto a new base in one chained operation.
func TestLabRebaseMany(t *testing.T) {
	s := newLabServer(t)
	admin := &labClient{t: t, s: s, token: "lab-admin"}
	s.tokens = map[string]bool{"lab-admin": true}

	// Create a repo with a linear mainline.
	admin.ok("POST", "/api/v1/repo", map[string]any{"namespace": "team", "name": "app"})
	admin.ok("POST", "/api/v1/repo/team/app/files", map[string]any{
		"changes":     []map[string]any{{"path": "m.txt", "content": "m\n"}},
		"description": "a",
		"new_commit":  true,
	})
	admin.ok("POST", "/api/v1/repo/team/app/files", map[string]any{
		"changes":     []map[string]any{{"path": "m2.txt", "content": "m2\n"}},
		"description": "b",
		"new_commit":  true,
	})

	// A feature branch off the tip.
	admin.ok("POST", "/api/v1/repo/team/app/branches", map[string]any{"name": "feat", "target": "@"})
	admin.ok("POST", "/api/v1/repo/team/app/files", map[string]any{
		"changes":     []map[string]any{{"path": "d.txt", "content": "d\n"}},
		"description": "d",
		"new_commit":  true,
	})
	admin.ok("POST", "/api/v1/repo/team/app/files", map[string]any{
		"changes":     []map[string]any{{"path": "g.txt", "content": "g\n"}},
		"description": "g",
		"new_commit":  true,
	})
	admin.ok("POST", "/api/v1/repo/team/app/files", map[string]any{
		"changes":     []map[string]any{{"path": "h.txt", "content": "h\n"}},
		"description": "h",
		"new_commit":  true,
	})

	// Create a branch "tip" at the mainline tip for the onto target.
	rec := admin.do("POST", "/api/v1/repo/team/app/branches", map[string]any{"name": "onto", "target": "@"})
	_ = rec

	// Find revision ids by description: d and h.
	revResp := admin.do("GET", "/api/v1/repo/team/app/revisions", nil)
	if revResp.Code != http.StatusOK {
		t.Fatalf("list revisions: %d", revResp.Code)
	}
	var revs []map[string]any
	if err := json.Unmarshal(revResp.Body.Bytes(), &revs); err != nil {
		t.Fatal(err)
	}
	idByDesc := map[string]string{}
	for _, r := range revs {
		idByDesc[r["description"].(string)] = r["revision_id"].(string)
	}
	if idByDesc["d"] == "" || idByDesc["h"] == "" {
		t.Fatalf("missing d/h revisions: %v", idByDesc)
	}

	// Move [d, h] onto the mainline tip (drop g).
	resp := admin.do("POST", "/api/v1/repo/team/app/rebase-many", map[string]any{
		"revisions": []string{idByDesc["d"], idByDesc["h"]},
		"onto":      "onto",
	})
	if resp.Code != http.StatusOK {
		t.Fatalf("rebase-many: %d %s", resp.Code, resp.Body.String())
	}
}
