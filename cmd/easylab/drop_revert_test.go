package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// listRevisionsByDesc returns description -> revision_id for a repo.
func listRevisionsByDesc(t *testing.T, admin *labClient, repo string) map[string]string {
	t.Helper()
	resp := admin.do("GET", "/api/v1/repo/"+repo+"/revisions", nil)
	if resp.Code != http.StatusOK {
		t.Fatalf("list revisions: %d", resp.Code)
	}
	var revs []map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &revs); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range revs {
		out[r["description"].(string)] = r["revision_id"].(string)
	}
	return out
}

// TestLabDrop verifies /drop erases a middle revision from history.
func TestLabDrop(t *testing.T) {
	s := newLabServer(t)
	admin := &labClient{t: t, s: s, token: "lab-admin"}
	seedTestToken(t, s, "lab-admin")
	admin.ok("POST", "/api/v1/repo", map[string]any{"namespace": "team", "name": "app"})

	commit := func(path, content, desc, parentHash string) string {
		body := map[string]any{"changes": []map[string]any{{"path": path, "content": content}}, "description": desc, "new_commit": true}
		if parentHash != "" {
			body["parent_hash"] = parentHash
		}
		m := admin.ok("POST", "/api/v1/repo/team/app/files", body)
		return m["snapshot"].(string)
	}
	sa := commit("a.txt", "a\n", "a", "")
	sb := commit("b.txt", "b\n", "b", sa)
	sc := commit("c.txt", "c\n", "c", sb)
	sd := commit("d.txt", "d\n", "d", sc)
	_ = sd

	// Point "main" branch at the current tip (the d revision) EXPLICITLY,
	// so the tree lookup below reflects post-drop state deterministically.
	desc := listRevisionsByDesc(t, admin, "team/app")
	admin.ok("POST", "/api/v1/repo/team/app/branches", map[string]any{"name": "main", "target": desc["d"]})

	cid := desc["c"]

	drop := admin.do("POST", "/api/v1/repo/team/app/revisions/"+cid+"/drop", nil)
	if drop.Code != http.StatusOK {
		t.Fatalf("drop: %d %s", drop.Code, drop.Body.String())
	}
	// The moved tip (d, rebased onto b) tree should have a,b,d and no c.
	var dropResp map[string]any
	if err := json.Unmarshal(drop.Body.Bytes(), &dropResp); err != nil {
		t.Fatal(err)
	}
	moved := dropResp["moved"].([]any)
	if len(moved) != 1 {
		t.Fatalf("expected 1 moved revision, got %d", len(moved))
	}
	tipID := moved[0].(map[string]any)["revision_id"].(string)
	treeResp := admin.do("GET", "/api/v1/repo/team/app/tree?ref="+tipID, nil)
	if treeResp.Code != http.StatusOK {
		t.Fatalf("tree: %d", treeResp.Code)
	}
	body := treeResp.Body.String()
	if bodyContains(body, "c.txt") {
		t.Fatalf("c.txt should be gone after drop: %s", body)
	}
	if !bodyContains(body, "d.txt") {
		t.Fatalf("d.txt should remain after drop: %s", body)
	}
	_ = sa
}

// TestLabRevert verifies /revert preserves history while undoing a change.
func TestLabRevert(t *testing.T) {
	s := newLabServer(t)
	admin := &labClient{t: t, s: s, token: "lab-admin"}
	seedTestToken(t, s, "lab-admin")
	admin.ok("POST", "/api/v1/repo", map[string]any{"namespace": "team", "name": "rev"})

	commit := func(path, content, desc, parentHash string) string {
		body := map[string]any{"changes": []map[string]any{{"path": path, "content": content}}, "description": desc, "new_commit": true}
		if parentHash != "" {
			body["parent_hash"] = parentHash
		}
		m := admin.ok("POST", "/api/v1/repo/team/rev/files", body)
		return m["snapshot"].(string)
	}
	sa := commit("a.txt", "a\n", "a", "")
	sb := commit("b.txt", "b\n", "b", sa)
	sc := commit("c.txt", "c\n", "c", sb)
	sd := commit("d.txt", "d\n", "d", sc)
	_ = sd

	// Point "main" branch at the current tip (the d revision) EXPLICITLY.
	desc := listRevisionsByDesc(t, admin, "team/rev")
	admin.ok("POST", "/api/v1/repo/team/rev/branches", map[string]any{"name": "main", "target": desc["d"]})

	// Revert the "c" revision (added c.txt). History preserved.
	cid := desc["c"]
	rev := admin.do("POST", "/api/v1/repo/team/rev/revisions/"+cid+"/revert", nil)
	if rev.Code != http.StatusOK {
		t.Fatalf("revert: %d %s", rev.Code, rev.Body.String())
	}
	// The new tip undoes c: c.txt removed, d.txt kept.
	// Find the revert revision (description prefixed "Revert ").
	revertID := ""
	for desc, id := range listRevisionsByDesc(t, admin, "team/rev") {
		if len(desc) >= 6 && desc[:6] == "Revert" {
			revertID = id
			break
		}
	}
	if revertID == "" {
		t.Fatal("no revert revision created")
	}
	treeResp := admin.do("GET", "/api/v1/repo/team/rev/tree?ref="+revertID, nil)
	if treeResp.Code != http.StatusOK {
		t.Fatalf("tree: %d", treeResp.Code)
	}
	body := treeResp.Body.String()
	if bodyContains(body, "c.txt") {
		t.Fatalf("c.txt should be removed after revert: %s", body)
	}
	if !bodyContains(body, "d.txt") {
		t.Fatalf("d.txt should remain after revert: %s", body)
	}
	// History preserved: c still exists as a revision.
	if got := listRevisionsByDesc(t, admin, "team/rev"); got["c"] != cid {
		t.Fatalf("c should still exist after revert (history preserved): %v", got)
	}
}

func bodyContains(s, sub string) bool {
	return s != "" && containsStr(s, sub)
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
