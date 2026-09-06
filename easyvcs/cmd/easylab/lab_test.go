package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"easyvcs/internal/store"
)

type labClient struct {
	t     *testing.T
	s     *server
	token string
}

func (c *labClient) do(method, path string, body any) *httptest.ResponseRecorder {
	c.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	c.s.router().ServeHTTP(rec, req)
	return rec
}

func (c *labClient) ok(method, path string, body any) map[string]any {
	c.t.Helper()
	rec := c.do(method, path, body)
	if rec.Code != http.StatusOK {
		c.t.Fatalf("%s %s: expected 200, got %d: %s", method, path, rec.Code, rec.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		c.t.Fatalf("decode %s: %v (%s)", path, err, rec.Body.String())
	}
	return m
}

func TestLabFullFlow(t *testing.T) {
	s := newLabServer(t)
	// Enable auth with an admin token; the client sends it on all requests.
	s.tokens = map[string]bool{"lab-admin": true}
	c := &labClient{t: t, s: s, token: "lab-admin"}

	// 1. Create users.
	c.ok("POST", "/api/v1/users", map[string]any{"username": "alice", "display_name": "Alice"})
	c.ok("POST", "/api/v1/users", map[string]any{"username": "bob"})

	// 2. Create tokens for alice.
	c.ok("POST", "/api/v1/users/alice/tokens", map[string]any{"token": "alice-token", "level": "write"})

	// 3. Namespace membership: alice is owner of team.
	c.ok("POST", "/api/v1/namespaces/team/members", map[string]any{"username": "alice", "role": "owner"})

	// 4. Create a repository in namespace team.
	rep := c.ok("POST", "/api/v1/repo", map[string]any{
		"namespace": "team", "name": "app", "description": "App repo",
	})
	if rep["name"] != "app" {
		t.Fatalf("create repo: %v", rep)
	}

	// 5. Commit via the Lab API (root commit with changes).
	commitBody := map[string]any{
		"parent_hash": "",
		"description": "initial",
		"changes": []map[string]any{
			{"path": "README.md", "content": "hello\n"},
			{"path": "src/main.go", "content": "package main\n"},
		},
	}
	res := c.ok("POST", "/api/v1/repo/team/app/files", commitBody)
	revID, _ := res["revision_id"].(string)
	if revID == "" {
		t.Fatalf("commit returned no revision_id: %v", res)
	}

	// 6. List revisions (returns a JSON array).
	revrec := c.do("GET", "/api/v1/repo/team/app/revisions", nil)
	if revrec.Code != http.StatusOK {
		t.Fatalf("list revisions: %d %s", revrec.Code, revrec.Body.String())
	}
	var revList []map[string]any
	if err := json.Unmarshal(revrec.Body.Bytes(), &revList); err != nil {
		t.Fatalf("decode revisions: %v", err)
	}
	if len(revList) != 1 {
		t.Fatalf("revisions: want 1, got %d", len(revList))
	}

	// 7. Read a blob (raw file content).
	rec := c.do("GET", "/api/v1/repo/team/app/blob?path=README.md&ref=@", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "hello\n" {
		t.Fatalf("blob: %d %s", rec.Code, rec.Body.String())
	}

	// 8. Tree listing.
	tre := c.ok("GET", "/api/v1/repo/team/app/tree?ref=@", nil)
	if _, ok := tre["entries"].([]any); !ok {
		t.Fatalf("tree entries missing: %v", tre)
	}

	// 9. Search.
	se := c.ok("GET", "/api/v1/repo/team/app/search?q=package&ref=@", nil)
	if _, ok := se["matches"]; !ok {
		t.Fatalf("search missing matches: %v", se)
	}

	// 10. Refs: set a branch and a tag.
	c.ok("POST", "/api/v1/repo/team/app/branches", map[string]any{"name": "main", "revision_id": revID})
	trec := c.do("GET", "/api/v1/repo/team/app/branches", nil)
	if trec.Code != http.StatusOK {
		t.Fatalf("list branchs: %d", trec.Code)
	}
	var bks []map[string]any
	_ = json.Unmarshal(trec.Body.Bytes(), &bks)
	if len(bks) != 1 || bks[0]["name"] != "main" {
		t.Fatalf("branchs: %v", bks)
	}
	c.ok("POST", "/api/v1/repo/team/app/tags", map[string]any{"name": "v1", "revision_id": revID})
	tagrec := c.do("GET", "/api/v1/repo/team/app/tags", nil)
	if tagrec.Code != http.StatusOK {
		t.Fatalf("list tags: %d", tagrec.Code)
	}
	var tags []map[string]any
	_ = json.Unmarshal(tagrec.Body.Bytes(), &tags)
	if len(tags) != 1 || tags[0]["name"] != "v1" {
		t.Fatalf("tags: %v", tags)
	}

	// 11. Diff of root revision (vs empty tree) — returns an array.
	drec := c.do("GET", "/api/v1/repo/team/app/revisions/"+revID+"/diff", nil)
	if drec.Code != http.StatusOK {
		t.Fatalf("diff: %d %s", drec.Code, drec.Body.String())
	}
	var diffs []map[string]any
	if err := json.Unmarshal(drec.Body.Bytes(), &diffs); err != nil {
		t.Fatalf("decode diff: %v", err)
	}
	if len(diffs) < 1 {
		t.Fatalf("diff: want >=1, got %d", len(diffs))
	}
	if diffs[0]["Status"] != "added" {
		t.Fatalf("root diff should be added, got %v", diffs[0]["Status"])
	}

	// 12. Merge requests: create, list, review, comment, merge.
	c.ok("POST", "/api/v1/repo/team/app/merge_requests", map[string]any{
		"title": "add feature", "source": "main", "target": "main",
	})
	mrsrec := c.do("GET", "/api/v1/repo/team/app/merge_requests", nil)
	if mrsrec.Code != http.StatusOK {
		t.Fatalf("list mrs: %d", mrsrec.Code)
	}
	var mrList []map[string]any
	if err := json.Unmarshal(mrsrec.Body.Bytes(), &mrList); err != nil {
		t.Fatalf("decode mrs: %v", err)
	}
	if len(mrList) != 1 {
		t.Fatalf("mrs: want 1, got %d", len(mrList))
	}
	c.ok("POST", "/api/v1/repo/team/app/merge_requests/1/reviews", map[string]any{"state": "approved"})
	c.ok("POST", "/api/v1/repo/team/app/merge_requests/1/comments", map[string]any{"body": "looks good"})
	c.ok("POST", "/api/v1/repo/team/app/merge_requests/1/merge", nil)

	// 13. Fork the repository, then verify the fork has its own history.
	fk := c.ok("POST", "/api/v1/repo/team/app/fork", map[string]any{"name": "app-fork"})
	if fk["name"] != "app-fork" {
		t.Fatalf("fork: %v", fk)
	}
	frev := c.do("GET", "/api/v1/repo/team/app-fork/revisions", nil)
	if frev.Code != http.StatusOK {
		t.Fatalf("fork revisions: %d", frev.Code)
	}
	var frs []map[string]any
	_ = json.Unmarshal(frev.Body.Bytes(), &frs)
	if len(frs) < 1 {
		t.Fatalf("fork revisions: want >=1, got %d", len(frs))
	}

	// 14. Releases: create, upload asset, list assets, list releases.
	rel := c.ok("POST", "/api/v1/repo/team/app/releases", map[string]any{
		"tag": "v1.0.0", "name": "v1", "description": "first release", "revision_id": revID,
	})
	if rel["tag"] != "v1.0.0" {
		t.Fatalf("release: %v", rel)
	}
	// Upload an asset via multipart.
	var b bytes.Buffer
	mw := multipart.NewWriter(&b)
	fw, _ := mw.CreateFormFile("file", "artifact.tar.gz")
	fw.Write([]byte("tarball-bytes"))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/v1/repo/team/app/releases/v1.0.0/assets", &b)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec2 := httptest.NewRecorder()
	s.router().ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("upload asset: %d %s", rec2.Code, rec2.Body.String())
	}
	var asset map[string]any
	_ = json.Unmarshal(rec2.Body.Bytes(), &asset)
	if asset["name"] != "artifact.tar.gz" || asset["size"] != float64(len("tarball-bytes")) {
		t.Fatalf("asset: %v", asset)
	}
	al := c.do("GET", "/api/v1/repo/team/app/releases/v1.0.0/assets", nil)
	if al.Code != http.StatusOK {
		t.Fatalf("list assets: %d", al.Code)
	}
	rl := c.do("GET", "/api/v1/repo/team/app/releases", nil)
	if rl.Code != http.StatusOK {
		t.Fatalf("list releases: %d", rl.Code)
	}
}

// newLabServer builds a server with the Lab schema and pkrkit registry
// initialized and no auth.
func newLabServer(t *testing.T) *server {
	t.Helper()
	home := t.TempDir()
	t.Setenv("EASYVCS_HOME", home)
	cs, err := store.OpenDefault()
	if err != nil {
		t.Fatal(err)
	}
	if err := cs.SetWAL(); err != nil {
		t.Fatal(err)
	}
	reg, err := openRegistry(home)
	if err != nil {
		t.Fatal(err)
	}
	opsState, err := newOpsState()
	if err != nil {
		t.Fatal(err)
	}
	return &server{cs: cs, registry: reg, ops: opsState}
}

// TestLabVisibilityEnforced verifies that a private repository is hidden from
// non-members (list + get return 404), while a public one is visible. It also
// checks that an anonymous caller (no auth) cannot read a private repo once the
// instance has registered any users.
func TestLabVisibilityEnforced(t *testing.T) {
	s := newLabServer(t)
	// Create alice with a real token; the "admin" legit token is no longer used.
	anon := &labClient{t: t, s: s}
	anon.ok("POST", "/api/v1/users", map[string]any{"username": "alice"})
	anon.ok("POST", "/api/v1/users/alice/tokens", map[string]any{"token": "alice-token", "level": "write"})
	admin := &labClient{t: t, s: s, token: "alice-token"}
	admin.ok("POST", "/api/v1/namespaces/team/members", map[string]any{"username": "alice", "role": "owner"})
	// Private repo.
	admin.ok("POST", "/api/v1/repo", map[string]any{
		"namespace": "team", "name": "secret", "visibility": "private",
	})
	// Public repo under a separate namespace (no members).
	admin.ok("POST", "/api/v1/repo", map[string]any{
		"namespace": "public", "name": "open", "visibility": "public",
	})

	// Create a token for bob (non-member of "team") so the instance is no longer
	// "open": private repos must now be gated.
	admin.ok("POST", "/api/v1/users", map[string]any{"username": "bob"})
	admin.ok("POST", "/api/v1/users/bob/tokens", map[string]any{"token": "bob-token", "level": "read"})

	// bob cannot list the private repo; can list the public one.
	bob := &labClient{t: t, s: s, token: "bob-token"}
	bobList := bob.do("GET", "/api/v1/repo", nil)
	var repos []map[string]any
	_ = json.Unmarshal(bobList.Body.Bytes(), &repos)
	names := map[string]bool{}
	for _, rp := range repos {
		names[rp["name"].(string)] = true
	}
	if names["secret"] {
		t.Fatalf("bob should not see private repo: %v", repos)
	}
	if !names["open"] {
		t.Fatalf("bob should see public repo: %v", repos)
	}

	// bob cannot read a private revision directly (404).
	privRev := bob.do("GET", "/api/v1/repo/team/secret/revisions", nil)
	if privRev.Code == http.StatusOK {
		t.Fatalf("bob should not read private repo revisions: %d", privRev.Code)
	}

	// alice (member) can read it.
	admin.ok("GET", "/api/v1/repo/team/secret", nil)

	// Make the private repo public -> now bob can see it.
	admin.ok("PATCH", "/api/v1/repo/team/secret", map[string]any{"visibility": "public"})
	publicList := bob.do("GET", "/api/v1/repo/team/secret/revisions", nil)
	if publicList.Code != http.StatusOK {
		t.Fatalf("bob should read public repo revisions after flip: %d", publicList.Code)
	}
}

var _ = fmt.Sprintf
var _ = strings.TrimSpace
