package main

import (
	"context"
	"fmt"
	"strconv"
	"testing"

	"connectrpc.com/connect"

	artifactkit "github.com/easylab-platform/artifact/core"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easylab/internal/sbxreg"
	"github.com/easylab-platform/easyvcs/store"
)

// ---- fixtures: a 3-user world over a public repo + a private repo ----

type rbacWorld struct {
	s      *server
	owner  *store.User
	maint  *store.User
	dev    *store.User
	outsid *store.User
	repo   *store.Repo // public repo team/pub
	priv   *store.Repo // private repo team/secret
}

func newRBACWorld(t *testing.T) *rbacWorld {
	t.Helper()
	s := newTestServer(t)
	owner, _ := s.cs.CreateUser("owner", "Owner")
	maint, _ := s.cs.CreateUser("maint", "Maint")
	dev, _ := s.cs.CreateUser("dev", "Dev")
	outsider, _ := s.cs.CreateUser("outsider", "Outsider")

	pub, err := s.cs.Create(store.RepoRef{Owner: owner.ID, Namespace: "team", Name: "pub"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.cs.SetRepoMember(pub.RepoID(), maint.ID, store.RoleMaintainer, &owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.cs.SetRepoMember(pub.RepoID(), dev.ID, store.RoleDeveloper, &owner.ID); err != nil {
		t.Fatal(err)
	}
	priv, err := s.cs.Create(store.RepoRef{Owner: owner.ID, Namespace: "team", Name: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := priv.UpdateRepoMeta(store.RepoMeta{Visibility: "private", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	return &rbacWorld{s: s, owner: owner, maint: maint, dev: dev, outsid: outsider, repo: pub, priv: priv}
}

// principalCtx builds a call context as the auth interceptor would.
func principalCtx(u *store.User) context.Context {
	if u == nil {
		return withPrincipal(context.Background(), Principal{Anonymous: true})
	}
	return withPrincipal(context.Background(), Principal{UserID: u.ID, Username: u.Username})
}

func adminCtx() context.Context {
	return withPrincipal(context.Background(), Principal{Admin: true, Username: "admin"})
}

// ---- repository role resolution (unit-level, both directories) ----

func TestRoleOfRepoPublicAndPrivate(t *testing.T) {
	w := newRBACWorld(t)
	pubID, privID := w.repo.RepoID(), w.priv.RepoID()

	// Public repo: owner=owner, maint=maintainer, dev=developer, outsider=dev-read fallback.
	cases := []struct {
		name   string
		repoID int64
		user   *store.User
		anon   bool
		want   store.Role
	}{
		{"public/owner", pubID, w.owner, false, store.RoleOwner},
		{"public/maint", pubID, w.maint, false, store.Role(store.RoleMaintainer)},
		{"public/dev", pubID, w.dev, false, store.Role(store.RoleDeveloper)},
		{"public/outsider", pubID, w.outsid, false, store.Role(store.RoleDeveloper)},
		{"public/anon", pubID, nil, true, store.Role(store.RoleDeveloper)},
		{"private/owner", privID, w.owner, false, store.RoleOwner},
		{"private/outsider", privID, w.outsid, false, store.RoleNone},
		{"private/anon", privID, nil, true, store.RoleNone},
	}
	for _, c := range cases {
		uid := int64(0)
		if c.user != nil {
			uid = c.user.ID
		}
		got, err := w.s.cs.RoleOf(c.repoID, uid)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: role=%q want %q", c.name, got, c.want)
		}
	}
}

func TestRequireRepoActionMatrix(t *testing.T) {
	w := newRBACWorld(t)
	// read allows everyone (public); propose allows developer+; merge allows
	// maintainer+; push allows owner only.
	type tc struct {
		name string
		user *store.User
		prop func(store.Role) bool
		ok   bool
	}
	mk := func(u *store.User) context.Context {
		if u == nil {
			return principalCtx(nil)
		}
		return principalCtx(u)
	}
	for _, c := range []tc{
		{"read/anon", nil, repoCanRead, true},
		{"read/outsider", w.outsid, repoCanRead, true},
		{"propose/dev", w.dev, repoCanPropose, true},
		{"propose/outsider", w.outsid, repoCanPropose, true}, // public→developer
		{"merge/maintainer", w.maint, repoCanMerge, true},
		{"merge/dev", w.dev, repoCanMerge, false},
		{"push/owner", w.owner, repoCanPush, true},
		{"push/maintainer", w.maint, repoCanPush, false},
		{"push/dev", w.dev, repoCanPush, false},
	} {
		err := w.s.requireRepoAction(mk(c.user), "team", "pub", c.prop, "test")
		if c.ok && err != nil {
			t.Fatalf("%s: expected allow, got %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("%s: expected deny, got allow", c.name)
		}
	}
}

// ---- package roles inherit from repo ----

func TestPackageScopeRoleFollowsRepo(t *testing.T) {
	w := newRBACWorld(t)
	// npm @team/pub maps to repo team/pub.
	if _, ok := w.s.cs.RepoForPackage("npm", "@team/pub"); !ok {
		t.Fatal("npm @team/pub should map to repo team/pub")
	}
	roleOf := func(u *store.User) store.Role {
		r, err := w.s.cs.PackageScopeRole("npm", "@team/pub", u.ID)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := roleOf(w.maint); !r.CanMerge() {
		t.Fatalf("maintainer should manage package, got %q", r)
	}
	if r := roleOf(w.dev); r.CanMerge() {
		t.Fatalf("developer must not manage package, got %q", r)
	}
	// Unclaimed name → public read, no manage.
	if !w.s.cs.CanRead(context.Background(), "npm", "lodash", w.outsid.ID) {
		t.Fatal("unclaimed package must be public-readable")
	}
	if r, _ := w.s.cs.PackageScopeRole("npm", "lodash", w.outsid.ID); r.CanMerge() {
		t.Fatal("unclaimed package must not grant management")
	}
}

func TestSetPackageVisibilityAuthorized(t *testing.T) {
	w := newRBACWorld(t)
	ctx := context.Background()
	// Maintainer publishes + flips visibility.
	if err := w.s.cs.AuthorizePublish(ctx, "npm", "@team/pub", w.maint.ID); err != nil {
		t.Fatalf("maintainer publish: %v", err)
	}
	if err := w.s.cs.SetPackageVisibilityAuthorized("npm", "@team/pub", w.maint.ID, "private"); err != nil {
		t.Fatalf("maintainer visibility flip: %v", err)
	}
	if w.s.cs.CanRead(ctx, "npm", "@team/pub", w.outsid.ID) {
		t.Fatal("outsider must not read a private package")
	}
	if !w.s.cs.CanRead(ctx, "npm", "@team/pub", w.maint.ID) {
		t.Fatal("maintainer must read own private package")
	}
	// Developer cannot flip.
	if err := w.s.cs.SetPackageVisibilityAuthorized("npm", "@team/pub", w.dev.ID, "public"); err == nil {
		t.Fatal("developer visibility flip must be denied")
	}
}

// ---- service visibility/operation (pure helpers, no k8s) ----

func TestServiceRoleVisibility(t *testing.T) {
	w := newRBACWorld(t)
	pubOwner := strconv.FormatInt(w.owner.ID, 10)

	// Repo-bound public service: outsider/anon can see; only maintainer+ operates.
	if !w.s.canSeeService(principalCtx(w.outsid), pubOwner, "team", "pub") {
		t.Fatal("outsider should see a public repo service")
	}
	if w.s.canSeeService(principalCtx(nil), pubOwner, "team", "pub") {
		t.Fatal("anonymous must not see a service")
	}
	if !w.s.canOperateService(principalCtx(w.maint), pubOwner, "team", "pub") {
		t.Fatal("maintainer should operate repo service")
	}
	if w.s.canOperateService(principalCtx(w.dev), pubOwner, "team", "pub") {
		t.Fatal("developer must not operate repo service")
	}
	// Standalone service (no org/repo): only its recorded owner.
	standaloneOwner := strconv.FormatInt(w.dev.ID, 10)
	if !w.s.canOperateService(principalCtx(w.dev), standaloneOwner, "", "") {
		t.Fatal("standalone owner should operate own service")
	}
	if w.s.canOperateService(principalCtx(w.outsid), standaloneOwner, "", "") {
		t.Fatal("non-owner must not operate a standalone service")
	}
	// Admin sees/operates everything.
	if !w.s.canSeeService(adminCtx(), "", "", "") || !w.s.canOperateService(adminCtx(), "", "", "") {
		t.Fatal("admin must see and operate any service")
	}
}

// ---- sandbox owner-only (in-memory registry, no k8s) ----

func TestSandboxOwnerOnly(t *testing.T) {
	w := newRBACWorld(t)
	cs := &connSandbox{s: w.s}
	// Owner-bound sandbox.
	if err := w.s.sbx.Upsert(sbxreg.Sandbox{Name: "sbx-a", Mode: "managed", OwnerUserID: w.owner.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.requireSandboxAccess(principalCtx(w.owner), "sbx-a"); err != nil {
		t.Fatalf("owner should access own sandbox: %v", err)
	}
	if _, err := cs.requireSandboxAccess(principalCtx(w.dev), "sbx-a"); err == nil {
		t.Fatal("non-owner must not access another user's sandbox")
	}
	if _, err := cs.requireSandboxAccess(adminCtx(), "sbx-a"); err != nil {
		t.Fatalf("admin should access any sandbox: %v", err)
	}
	// Legacy unowned repo-bound sandbox: repo readers get access.
	if err := w.s.sbx.Upsert(sbxreg.Sandbox{Name: "sbx-legacy", Org: "team", Repo: "pub"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.requireSandboxAccess(principalCtx(w.outsid), "sbx-legacy"); err != nil {
		t.Fatalf("public repo reader should access legacy sandbox: %v", err)
	}
	// Legacy unowned standalone sandbox: nobody but admin.
	if err := w.s.sbx.Upsert(sbxreg.Sandbox{Name: "sbx-legacy2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cs.requireSandboxAccess(principalCtx(w.owner), "sbx-legacy2"); err == nil {
		t.Fatal("unowned standalone sandbox must deny everyone but admin")
	}
	// ListByOwner returns only the caller's rows (+ unowned when asked).
	rows, err := w.s.sbx.ListByOwner(w.owner.ID, false)
	if err != nil || len(rows) != 1 || rows[0].Name != "sbx-a" {
		t.Fatalf("ListByOwner(owner) = %+v (%v)", rows, err)
	}
}

// ---- LabService: named capability gate through the real RPC ----

func labCall(t *testing.T, s *server, ctx context.Context, fn func(context.Context) error) error {
	t.Helper()
	_ = ctx
	return fn(ctx)
}

func TestConnLabCreateRepoOwnerAndDenyAnon(t *testing.T) {
	s := newTestServer(t)
	cl := &connLab{s}
	// Anonymous cannot create.
	_, err := cl.CreateRepo(principalCtx(nil), connect.NewRequest(&easylabv1.CreateRepoRequest{Org: "team", Repo: "x"}))
	if err == nil || connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous CreateRepo should be unauthenticated, got %v", err)
	}
	// Authenticated creates an owned repo.
	u, _ := s.cs.CreateUser("alice", "Alice")
	if _, err := cl.CreateRepo(principalCtx(u), connect.NewRequest(&easylabv1.CreateRepoRequest{Org: "team", Repo: "x"})); err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	repo, err := s.cs.OpenRepo(store.RepoRef{Namespace: "team", Name: "x"})
	if err != nil || repo.OwnerUserID != u.ID {
		t.Fatalf("repo owner = %+v (%v), want %d", repo, err, u.ID)
	}
}

func TestConnLabWriteBlobRoleGate(t *testing.T) {
	w := newRBACWorld(t)
	cl := &connLab{w.s}
	// Developer (public repo) may NOT push a branch (gate fires before any
	// branch lookup).
	_, err := cl.WriteBlob(principalCtx(w.dev), connect.NewRequest(&easylabv1.WriteBlobRequest{
		Org: "team", Repo: "pub", Path: "a.txt", Content: "hi",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("developer WriteBlob should be permission_denied, got %v", err)
	}
	// Developer may PROPOSE (CanPropose) but not push: assert the propose-gate
	// itself passes for a developer by exercising CreateBranch's push gate
	// separately below. Here we confirm the owner clears the propose gate.
	if _, err := cl.WriteBlob(principalCtx(w.owner), connect.NewRequest(&easylabv1.WriteBlobRequest{
		Org: "team", Repo: "pub", Path: "a.txt", Content: "hi",
	})); connect.CodeOf(err) == connect.CodePermissionDenied {
		t.Fatalf("owner WriteBlob should not be permission_denied: %v", err)
	}
}

func TestConnLabCreateBranchOwnerOnly(t *testing.T) {
	w := newRBACWorld(t)
	cl := &connLab{w.s}
	// Creating a branch is a direct ref write: owner only.
	_, err := cl.CreateBranch(principalCtx(w.maint), connect.NewRequest(&easylabv1.CreateBranchRequest{
		Org: "team", Repo: "pub", Branch: "feature", From: "main",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("maintainer CreateBranch should be denied, got %v", err)
	}
	_, err = cl.CreateBranch(principalCtx(w.dev), connect.NewRequest(&easylabv1.CreateBranchRequest{
		Org: "team", Repo: "pub", Branch: "feature", From: "main",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("developer CreateBranch should be denied, got %v", err)
	}
}

func TestConnLabDeleteRepoOwnerOnly(t *testing.T) {
	w := newRBACWorld(t)
	cl := &connLab{w.s}
	// Maintainer cannot delete the repo.
	_, err := cl.DeleteRepo(principalCtx(w.maint), connect.NewRequest(&easylabv1.DeleteRepoRequest{Org: "team", Repo: "pub"}))
	if err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("maintainer DeleteRepo should be denied, got %v", err)
	}
	// Owner can.
	if _, err := cl.DeleteRepo(principalCtx(w.owner), connect.NewRequest(&easylabv1.DeleteRepoRequest{Org: "team", Repo: "pub"})); err != nil {
		t.Fatalf("owner DeleteRepo: %v", err)
	}
}

// ---- UserService admin gate ----

func TestConnUserAdminGate(t *testing.T) {
	s := newTestServer(t)
	cu := &connUser{s}
	// Non-admin cannot create a user.
	_, err := cu.CreateUser(principalCtx(&store.User{ID: 999, Username: "x"}), connect.NewRequest(&easylabv1.CreateUserRequest{Username: "newbie"}))
	if err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-admin CreateUser should be denied, got %v", err)
	}
	// Admin can, and gets a one-time token.
	res, err := cu.CreateUser(adminCtx(), connect.NewRequest(&easylabv1.CreateUserRequest{Username: "newbie"}))
	if err != nil {
		t.Fatalf("admin CreateUser: %v", err)
	}
	if res.Msg.Token == "" || res.Msg.User.Username != "newbie" {
		t.Fatalf("CreateUser response = %+v", res.Msg)
	}
	// DeleteUser cascades.
	id, _ := strconv.ParseInt(res.Msg.User.Id, 10, 64)
	if _, err := cu.DeleteUser(adminCtx(), connect.NewRequest(&easylabv1.DeleteUserRequest{Id: res.Msg.User.Id})); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, err := s.cs.GetUser(id); err == nil {
		t.Fatal("user should be deleted")
	}
}

// ---- RegistryService: visibility RPC + list filter ----

func TestConnRegistryListAndVisibility(t *testing.T) {
	w := newRBACWorld(t)
	cr := &connRegistry{w.s}
	ctx := context.Background()
	// Maintainer claims a package under the mapped repo and publishes a version
	// (so it appears in ListRepositories).
	if err := w.s.cs.AuthorizePublish(ctx, "npm", "@team/pub", w.maint.ID); err != nil {
		t.Fatal(err)
	}
	if err := w.s.registry.Meta.Put(ctx, artifactkit.Artifact{Format: "npm", Repository: "@team/pub", Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	// Flip private via the RPC as owner.
	if _, err := cr.SetPackageVisibility(principalCtx(w.owner), connect.NewRequest(&easylabv1.SetPackageVisibilityRequest{
		Type: "npm", Name: "@team/pub", Visibility: "private",
	})); err != nil {
		t.Fatalf("owner SetPackageVisibility: %v", err)
	}
	// Developer RPC flip denied.
	if _, err := cr.SetPackageVisibility(principalCtx(w.dev), connect.NewRequest(&easylabv1.SetPackageVisibilityRequest{
		Type: "npm", Name: "@team/pub", Visibility: "public",
	})); err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("developer SetPackageVisibility should be denied, got %v", err)
	}
	// ListPackages as outsider must NOT include the now-private package.
	res, err := cr.ListPackages(principalCtx(w.outsid), connect.NewRequest(&easylabv1.ListPackagesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Msg.Packages {
		if p.Name == "@team/pub" {
			t.Fatal("outsider must not list a private package")
		}
	}
	// ListPackages as maintainer DOES include it, with visibility=private + owner set.
	resM, err := cr.ListPackages(principalCtx(w.maint), connect.NewRequest(&easylabv1.ListPackagesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range resM.Msg.Packages {
		if p.Name == "@team/pub" {
			found = true
			if p.Visibility != "private" {
				t.Fatalf("visibility = %q, want private", p.Visibility)
			}
			if p.Owner == "" {
				t.Fatal("owner should be set on a claimed package")
			}
		}
	}
	if !found {
		t.Fatal("maintainer should list the private package")
	}
}

// ---- WorkflowService: maintainer-only run/define ----

func TestConnWorkflowGates(t *testing.T) {
	w := newRBACWorld(t)
	cw := NewWorkflowService(w.s)
	// Maintainer defines a workflow on the repo.
	res, err := cw.CreateWorkflow(principalCtx(w.maint), connect.NewRequest(&easylabv1.CreateWorkflowRequest{
		Workflow: &easylabv1.Workflow{Name: "ci", Org: "team", Repo: "pub"},
	}))
	if err != nil {
		t.Fatalf("maintainer CreateWorkflow: %v", err)
	}
	// Developer cannot define.
	if _, err := cw.CreateWorkflow(principalCtx(w.dev), connect.NewRequest(&easylabv1.CreateWorkflowRequest{
		Workflow: &easylabv1.Workflow{Name: "ci2", Org: "team", Repo: "pub"},
	})); err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("developer CreateWorkflow should be denied, got %v", err)
	}
	// Developer cannot trigger the maintainer's workflow.
	if _, err := cw.TriggerRun(principalCtx(w.dev), connect.NewRequest(&easylabv1.TriggerRunRequest{
		WorkflowId: res.Msg.Workflow.Id,
	})); err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("developer TriggerRun should be denied, got %v", err)
	}
	// Maintainer can trigger.
	if _, err := cw.TriggerRun(principalCtx(w.maint), connect.NewRequest(&easylabv1.TriggerRunRequest{
		WorkflowId: res.Msg.Workflow.Id,
	})); err != nil {
		t.Fatalf("maintainer TriggerRun: %v", err)
	}
	// RunWorkflowFile on the upstream repo: developer denied.
	if _, err := cw.RunWorkflowFile(principalCtx(w.dev), connect.NewRequest(&easylabv1.RunWorkflowFileRequest{
		Org: "team", Repo: "pub", Branch: "main",
	})); err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("developer RunWorkflowFile should be denied, got %v", err)
	}
}

var _ = fmt.Sprintf
