package main

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"
)

// TestRevisionResolveAndCompare is a regression for the RPC-era rev resolution:
// bare change-id prefixes (e.g. "018a51fd") and branch/tag names must resolve,
// and Compare/Diff must succeed over them (previously resolveRefAny only
// accepted full hashes/branch names, breaking the vcs-diff tool).
func TestRevisionResolveAndCompare(t *testing.T) {
	s := newTestServer(t)
	u, _ := s.cs.CreateUser("dev", "Dev")
	repo, err := s.cs.Create(store.RepoRef{Owner: u.ID, Namespace: "team", Name: "cmp"})
	if err != nil {
		t.Fatal(err)
	}
	ws := revision.NewWorkspace(repo)
	snap1, ch1, err := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{
		{Path: "a.txt", Content: []byte("one\n")},
	}, "c1", store.Author{Name: "n"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.SetRef("main", store.RefBranch, snap1.RevisionHash.String()); err != nil {
		t.Fatal(err)
	}
	_, ch2, err := ws.CommitFromChanges(snap1.RevisionHash, []revision.FileChangeSpec{
		{Path: "a.txt", Content: []byte("two\n")},
	}, "c2", store.Author{Name: "n"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.SetRef("main", store.RefBranch, ch2.ID); err != nil {
		t.Fatal(err)
	}

	// resolveRefAny must accept a change-id PREFIX and a branch name.
	if _, err := resolveRefAny(ws, repo, ch1.ID[:8]); err != nil {
		t.Fatalf("resolveRefAny(prefix %s): %v", ch1.ID[:8], err)
	}
	if _, err := resolveRefAny(ws, repo, "main"); err != nil {
		t.Fatalf("resolveRefAny(main): %v", err)
	}

	// Compare over the two revisions (base → head) yields a diff.
	c := &connLab{s}
	res, err := c.Compare(principalCtx(u), connect.NewRequest(&easylabv1.CompareRequest{
		Org: "team", Repo: "cmp", From: ch1.ID, To: ch2.ID,
	}))
	if err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if len(res.Msg.GetFiles()) == 0 {
		t.Fatal("Compare returned no files")
	}

	// Diff by change-id prefix (the vcs-diff tool path via Diff RPC).
	if _, err := c.Diff(principalCtx(u), connect.NewRequest(&easylabv1.DiffRequest{
		Org: "team", Repo: "cmp", ChangeId: ch2.ID[:8],
	})); err != nil {
		t.Fatalf("Diff(prefix): %v", err)
	}
}

// TestBranchAndTagAndRepoConfig drives the new repo-management RPCs end to end.
func TestBranchAndTagAndRepoConfig(t *testing.T) {
	s := newTestServer(t)
	u, _ := s.cs.CreateUser("owner", "Owner")
	repo, err := s.cs.Create(store.RepoRef{Owner: u.ID, Namespace: "team", Name: "mgmt"})
	if err != nil {
		t.Fatal(err)
	}
	ws := revision.NewWorkspace(repo)
	snap, _, err := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{
		{Path: "f.txt", Content: []byte("x\n")},
	}, "init", store.Author{Name: "n"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.SetRef("main", store.RefBranch, snap.RevisionHash.String()); err != nil {
		t.Fatal(err)
	}
	c := &connLab{s}

	if _, err := c.CreateBranch(principalCtx(u), connect.NewRequest(&easylabv1.CreateBranchRequest{Org: "team", Repo: "mgmt", Branch: "feat", From: "main"})); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	br, err := c.Branches(principalCtx(u), connect.NewRequest(&easylabv1.BranchesRequest{Org: "team", Repo: "mgmt"}))
	if err != nil || len(br.Msg.GetBranches()) != 2 {
		t.Fatalf("Branches = %v (%v)", br, err)
	}
	if _, err := c.SetTag(principalCtx(u), connect.NewRequest(&easylabv1.SetTagRequest{Org: "team", Repo: "mgmt", Name: "v1"})); err != nil {
		t.Fatalf("SetTag: %v", err)
	}
	tags, err := c.Tags(principalCtx(u), connect.NewRequest(&easylabv1.TagsRequest{Org: "team", Repo: "mgmt"}))
	if err != nil || len(tags.Msg.GetTags()) != 1 {
		t.Fatalf("Tags = %v (%v)", tags, err)
	}
	priv := "private"
	if _, err := c.UpdateRepo(principalCtx(u), connect.NewRequest(&easylabv1.UpdateRepoRequest{Org: "team", Repo: "mgmt", Visibility: &priv})); err != nil {
		t.Fatalf("UpdateRepo: %v", err)
	}
	meta, _ := repo.RepoMeta()
	if meta.Visibility != "private" {
		t.Fatalf("visibility = %q", meta.Visibility)
	}
	if _, err := c.DeleteTag(principalCtx(u), connect.NewRequest(&easylabv1.DeleteTagRequest{Org: "team", Repo: "mgmt", Name: "v1"})); err != nil {
		t.Fatalf("DeleteTag: %v", err)
	}
	if _, err := c.DeleteBranch(principalCtx(u), connect.NewRequest(&easylabv1.DeleteBranchRequest{Org: "team", Repo: "mgmt", Branch: "feat"})); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
}

var _ = context.Background
