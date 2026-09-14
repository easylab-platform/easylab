package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// Gateway base for the RBAC HTTP checks (loopback inside the easylab pod).
var (
	labHTTP = envOr("EASYLAB", "http://127.0.0.1:8080")
	adminTok = envOr("EASYLAB_ADMIN_TOKEN", "admin-devtoken")
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// httpJSON POSTs a Connect/protobuf-JSON request with a bearer token.
func httpJSON(token, procedure string, body map[string]any) (int, map[string]any) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, labHTTP+procedure, bytes.NewReader(b))
	if err != nil {
		return 0, nil
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m
}

func rbacCheck(name string, got, want int) {
	if got == want {
		pass++
		fmt.Printf("PASS rbac/%-32s %d\n", name, got)
		return
	}
	fail++
	fmt.Printf("FAIL rbac/%-32s got %d want %d\n", name, got, want)
}

// runRBAC exercises the unified owner/maintainer/developer authorization over
// the live gateway: it provisions users + a repo via the admin/owner
// credentials, then asserts the role ladder for repo, package visibility,
// service deploy and CI run.
func runRBAC() {
	// Provision a fresh user e2e-<ts> (owns repos it creates), plus maint/dev.
	su := fmt.Sprintf("e2e%d", time.Now().UnixNano()%1000000)
	adminCreateUser := func(name string) string {
		code, body := httpJSON(adminTok, "/easylab.v1.UserService/CreateUser", map[string]any{"username": name})
		if code != 200 {
			fmt.Printf("NOTE rbac: CreateUser %s -> %d (may already exist)\n", name, code)
		}
		if u, ok := body["user"].(map[string]any); ok {
			if id, ok := u["id"].(string); ok {
				return id
			}
		}
		return ""
	}
	idO := adminCreateUser(su)
	idM := adminCreateUser(su + "m")
	idD := adminCreateUser(su + "d")

	adminTokenFor := func(name, tok string) string {
		httpJSON(adminTok, "/easylab.v1.UserService/CreateUserToken", map[string]any{"userId": name, "token": tok})
		return tok
	}
	ownerTok := adminTokenFor(idO, su+"-tok")
	maintTok := adminTokenFor(idM, su+"m-tok")
	devTok := adminTokenFor(idD, su+"d-tok")

	// Owner creates a repo and grants maint/dev (owner name == username).
	ns, repo := "team", su
	if code, _ := httpJSON(ownerTok, "/easylab.v1.LabService/CreateRepo", map[string]any{"org": ns, "repo": repo}); code != 200 {
		fmt.Printf("NOTE rbac: CreateRepo -> %d\n", code)
	}
	addMember := func(user, role string) {
		httpJSON(ownerTok, "/api/v1/namespaces/"+ns+"/members", map[string]any{"repo": repo, "username": user, "role": role})
	}
	addMember(su+"m", "maintainer")
	addMember(su+"d", "developer")

	// ---- auth boundary ----
	code, _ := httpJSON("", "/easylab.v1.LabService/Health", map[string]any{})
	rbacCheck("anon Health = 200", code, 200)
	code, _ = httpJSON("", "/easylab.v1.LabService/CreateRepo", map[string]any{"org": ns, "repo": "anon"})
	rbacCheck("anon CreateRepo = 401", code, 401)
	code, _ = httpJSON("nope", "/easylab.v1.LabService/CreateRepo", map[string]any{"org": ns, "repo": "anon"})
	rbacCheck("bad-token CreateRepo = 401", code, 401)

	// ---- repo write ladder: owner can, maint/dev cannot create a branch ----
	code, _ = httpJSON(ownerTok, "/easylab.v1.LabService/CreateBranch", map[string]any{"org": ns, "repo": repo, "branch": "rb", "from": "main"})
	rbacCheck("owner CreateBranch != 403", boolCode(code != 403), 200)
	code, _ = httpJSON(maintTok, "/easylab.v1.LabService/CreateBranch", map[string]any{"org": ns, "repo": repo, "branch": "mb", "from": "main"})
	rbacCheck("maintainer CreateBranch = 403", code, 403)
	code, _ = httpJSON(devTok, "/easylab.v1.LabService/CreateBranch", map[string]any{"org": ns, "repo": repo, "branch": "db", "from": "main"})
	rbacCheck("developer CreateBranch = 403", code, 403)

	// ---- UserService admin gate ----
	code, _ = httpJSON(ownerTok, "/easylab.v1.UserService/CreateUser", map[string]any{"username": "x"})
	rbacCheck("non-admin CreateUser = 403", code, 403)
	code, body := httpJSON(adminTok, "/easylab.v1.UserService/CreateUser", map[string]any{"username": su + "z"})
	rbacCheck("admin CreateUser = 200", code, 200)
	if s, _ := body["token"].(string); s != "" {
		pass++
		fmt.Println("PASS rbac/admin CreateUser returns token")
	} else {
		fail++
		fmt.Println("FAIL rbac/admin CreateUser returns token")
	}

	// ---- package visibility: maintainer+ only ----
	code, _ = httpJSON(devTok, "/easylab.v1.RegistryService/SetPackageVisibility", map[string]any{"type": "npm", "name": "@team/" + repo, "visibility": "private"})
	rbacCheck("developer SetPackageVisibility = 403", code, 403)

	// ---- CI run: developer denied on upstream repo ----
	code, _ = httpJSON(devTok, "/easylab.v1.WorkflowService/RunWorkflowFile", map[string]any{"org": ns, "repo": repo, "branch": "main"})
	rbacCheck("developer RunWorkflowFile = 403", code, 403)

	// ---- service deploy: developer denied, maintainer allowed ----
	code, _ = httpJSON(devTok, "/easylab.v1.OpsService/LaunchService", map[string]any{"name": su + "-svc", "image": "nginx", "org": ns, "repo": repo, "ports": []map[string]any{{"container": 80, "service": 80}}})
	rbacCheck("developer LaunchService = 403", code, 403)
	code, _ = httpJSON(maintTok, "/easylab.v1.OpsService/LaunchService", map[string]any{"name": su + "-svc", "image": "nginx", "org": ns, "repo": repo, "ports": []map[string]any{{"container": 80, "service": 80}}})
	rbacCheck("maintainer LaunchService = 200", code, 200)

	// cleanup
	httpJSON(maintTok, "/easylab.v1.OpsService/DeleteService", map[string]any{"name": su + "-svc"})
	httpJSON(ownerTok, "/easylab.v1.LabService/DeleteRepo", map[string]any{"org": ns, "repo": repo})
	httpJSON(adminTok, "/easylab.v1.UserService/DeleteUserToken", map[string]any{"token": su + "-tok"})
	httpJSON(adminTok, "/easylab.v1.UserService/DeleteUserToken", map[string]any{"token": su + "m-tok"})
	httpJSON(adminTok, "/easylab.v1.UserService/DeleteUserToken", map[string]any{"token": su + "d-tok"})
}

func boolCode(ok bool) int {
	if ok {
		return 200
	}
	return 0
}
