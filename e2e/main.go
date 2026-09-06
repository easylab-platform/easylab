// Command e2e drives EasyLab's agent extension surface over real NATS by
// simulating agent tool calls (abc.discover + tool.call.{ext}.{tool}),
// asserting correctness. It is modeled on zergx's deploy/e2e-live but targets
// the EasyLab extensions (easyvcs-code / easyvcs-ops) and easylab's aggregate
// API, with no mocks/inproc.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	natsbus "forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/transport/nats"
	"forgejo.develop.10.199.64.20.nip.io/abc-protocol/sdk-go/agent"
)

var (
	natsURL = envOr("E2E_NATS", "nats://127.0.0.1:14222")
	labBase = envOr("E2E_LAB", "http://127.0.0.1:18160")
)

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

var passed, failed int

func check(name string, ok bool, detail string) {
	if ok {
		passed++
		fmt.Printf("  PASS: %s\n", name)
	} else {
		failed++
		fmt.Printf("  FAIL: %s  (%s)\n", name, detail)
	}
}

func main() {
	bus, err := natsbus.Connect(natsURL)
	if err != nil {
		fmt.Println("nats connect:", err)
		os.Exit(1)
	}
	defer bus.Close()
	ag := agent.New(bus)
	ctx := context.Background()

	call := func(ext, tool string, args map[string]interface{}) (agent.ToolResult, error) {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return ag.CallTool(c, "e2e:workspace:main", ext, tool, "e2e-"+tool, args)
	}

	// ---- discovery: both extensions reachable ----
	ms, err := ag.Discover(ctx, 1500)
	check("discover reached extensions", err == nil && len(ms) >= 2, fmt.Sprintf("err=%v n=%d", err, len(ms)))
	extIDs := map[string]bool{}
	for _, m := range ms {
		extIDs[m.Id] = true
	}
	check("easyvcs-code present", extIDs["easyvcs-code"], fmt.Sprintf("ids=%v", extIDs))
	check("easyvcs-ops present", extIDs["easyvcs-ops"], fmt.Sprintf("ids=%v", extIDs))

	// ---- repos: list via easylab aggregate ----
	// (uses easylab /api/v1/repos through the agent HTTPS-less path; here we
	// verify the extension round-trip only, repo data via HTTP.)
	// We call easyvcs-code list against an existing repo (team/app).
	r, err := call("easyvcs-code", "list", map[string]interface{}{"org": "team", "repo": "app"})
	check("code.list", err == nil && strings.Contains(r.Content, "entries"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// read a file
	r, err = call("easyvcs-code", "read", map[string]interface{}{"org": "team", "repo": "app", "path": "README.md"})
	check("code.read", err == nil && r.Content != "", fmt.Sprintf("err=%v content=%q", err, r.Content))

	// refs
	r, err = call("easyvcs-code", "refs", map[string]interface{}{"org": "team", "repo": "app"})
	check("code.refs", err == nil, fmt.Sprintf("err=%v", err))

	// log
	r, err = call("easyvcs-code", "log", map[string]interface{}{"org": "team", "repo": "app"})
	check("code.log", err == nil && strings.Contains(r.Content, "revision"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// search
	r, err = call("easyvcs-code", "search", map[string]interface{}{"org": "team", "repo": "app", "q": "easyvcs"})
	check("code.search", err == nil, fmt.Sprintf("err=%v", err))

	// write -> new revision (in a throwaway repo to avoid mutating team/app)
	_, _ = createRepo(labBase, "e2ernd", "smoke")
	r, err = call("easyvcs-code", "write", map[string]interface{}{
		"org": "e2ernd", "repo": "smoke", "path": "x.txt", "content": "hello e2e\n", "message": "e2e write",
	})
	check("code.write", err == nil && strings.Contains(r.Content, "revision"), fmt.Sprintf("err=%v content=%q", err, r.Content))

	// ops: sandbox_services (list) — should succeed against easylab ops
	r, err = call("easyvcs-ops", "sandbox_services", map[string]interface{}{})
	check("ops.sandbox_services", err == nil, fmt.Sprintf("err=%v content=%q", err, r.Content))

	// ops: service status on the easylab service itself (deployed) is not
	// guaranteed; assert sandbox_services only. Build is heavy; skip by design.

	cleanupRepo(labBase, "e2ernd", "smoke")
	fmt.Printf("\nRESULT: %d passed, %d failed\n", passed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func createRepo(base, org, repo string) (bool, error) {
	req, _ := newReq("POST", base+"/api/v1/repositories", `{"namespace":"`+org+`","name":"`+repo+`"}`)
	resp, err := do(req)
	if resp != nil {
		resp.Body.Close()
	}
	return err == nil, err
}

func cleanupRepo(base, org, repo string) {
	req, _ := newReq("DELETE", base+"/api/v1/repositories/"+org+"/"+repo, "")
	if resp, err := do(req); err == nil && resp != nil {
		resp.Body.Close()
	}
}

func newReq(method, url, body string) (*http.Request, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		return nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func do(req *http.Request) (*http.Response, error) {
	return http.DefaultClient.Do(req)
}
