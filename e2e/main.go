// Synthetic agent-side e2e harness: connects straight to the cluster NATS,
// discovers the embedded ops/repo extensions and drives every tool over the
// abc wire (envelope in, tool result out) — the real agent container is not
// involved. Side effects hit the live temp-namespace easylab.
package main

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/abcp-sdk/abc-protocol-go/agent"
	buspkg "github.com/abcp-sdk/abc-protocol-go/bus"
	"github.com/abcp-sdk/abc-protocol-go/protocol"
	natsbus "github.com/abcp-sdk/abc-protocol-go/transport/nats"
)

const (
	session = "team:demo:main"
	tenant  = "team"
	extOps  = "ops"
	extRepo = "repo"
)

var (
	pass, fail, expErr int
	covered            = map[string]bool{}
	discovered         = map[string][]string{} // ext -> tools
)

func run(ag *agent.Agent, ext, tool string, timeout time.Duration, args map[string]any) (string, any) {
	covered[ext+"/"+tool] = true
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	start := time.Now()
	res, err := ag.CallTool(ctx, tenant, session, ext, tool, fmt.Sprintf("e2e-%d", time.Now().UnixNano()), args)
	d := time.Since(start).Round(time.Millisecond)
	if err != nil {
		fail++
		fmt.Printf("FAIL %-30s %8v wire: %v\n", ext+"/"+tool, d, err)
		return "", nil
	}
	if res.Error != nil {
		fail++
		fmt.Printf("FAIL %-30s %8v tool[%v]: %s\n", ext+"/"+tool, d, res.Error.Code, trunc(res.Error.Message))
		return "", res.Data
	}
	pass++
	fmt.Printf("PASS %-30s %8v %s\n", ext+"/"+tool, d, strings.ReplaceAll(trunc(res.Content), "\n", " ⏎ "))
	return res.Content, res.Data
}

// runExpectErr asserts the tool answers with a well-formed tool-level error
// (config-sensible for this fixture, e.g. missing branch) — wire path covered.
func runExpectErr(ag *agent.Agent, ext, tool string, timeout time.Duration, args map[string]any) {
	covered[ext+"/"+tool] = true
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := ag.CallTool(ctx, tenant, session, ext, tool, fmt.Sprintf("e2e-%d", time.Now().UnixNano()), args)
	if err != nil {
		fail++
		fmt.Printf("FAIL %-30s    wire: %v\n", ext+"/"+tool, err)
		return
	}
	if res.Error == nil {
		fail++
		fmt.Printf("FAIL %-30s    expected tool error, got: %s\n", ext+"/"+tool, trunc(res.Content))
		return
	}
	expErr++
	fmt.Printf("PASS %-30s    (expected error) %s\n", ext+"/"+tool, trunc(res.Error.Message))
}

func trunc(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

func dataStr(data any, key string) string {
	m, ok := data.(map[string]any)
	if !ok {
		return ""
	}
	if v, ok := m[key]; ok {
		return fmt.Sprint(v)
	}
	return ""
}

var shaRe = regexp.MustCompile(`\b[0-9a-f]{40}\b`)

func main() {
	url := os.Getenv("NATS")
	if url == "" {
		url = "nats://127.0.0.1:14222"
	}
	bus, err := natsbus.Connect(url)
	if err != nil {
		fmt.Println("nats:", err)
		os.Exit(1)
	}
	ag := agent.New(bus)

	mans, err := ag.Discover(context.Background(), 3000)
	if err != nil {
		fmt.Println("discover:", err)
		os.Exit(1)
	}
	for _, m := range mans {
		var names []string
		if m.Tools != nil {
			for _, t := range *m.Tools {
				names = append(names, t.Name)
			}
		}
		sort.Strings(names)
		discovered[m.Id] = names
		fmt.Printf("discovered %s v%s: %d tools\n", m.Id, m.Version, len(names))
	}
	for ext, tools := range discovered {
		fmt.Printf("  %s: %s\n", ext, strings.Join(tools, " "))
	}
	_, hasOps := discovered[extOps]
	_, hasRepo := discovered[extRepo]
	if !hasOps || !hasRepo {
		fmt.Println("ops/repo extensions missing")
		os.Exit(1)
	}

	// The repo extension materializes the session -> bookmark mapping from
	// lifecycle events; the real agent emits these on session create. Emit a
	// synthetic `created` so the tools have a workspace context. Lifecycle
	// subjects are tenant-namespaced: abc.<tenant>.session.lifecycle.<kind>.
	eid := fmt.Sprintf("e2e-%d", time.Now().UnixNano())
	if err := bus.Publish(context.Background(), protocol.ChLifecycle(tenant, "created"),
		map[string]any{"id": eid, "kind": "created", "session_name": session}, buspkg.PublishOpts{Tenant: tenant}); err != nil {
		fmt.Println("lifecycle publish:", err)
		os.Exit(1)
	}
	time.Sleep(700 * time.Millisecond)

	fmt.Println("\n===== repo extension =====")
	run(ag, extRepo, "explore", 30*time.Second, map[string]any{})
	run(ag, extRepo, "ls", 30*time.Second, map[string]any{"ref": "team:demo:main"})
	run(ag, extRepo, "write", 60*time.Second, map[string]any{
		"path": "e2e-tools.txt", "content": "line-one\nline-two\nline-three\n", "message": "e2e tools harness seed"})
	run(ag, extRepo, "write", 60*time.Second, map[string]any{
		"path": "Dockerfile", "content": "FROM docker.io/library/busybox:latest\nCOPY e2e-tools.txt /tmp/e2e-tools.txt\n", "message": "e2e: dockerfile for container-build"})
	run(ag, extRepo, "write", 60*time.Second, map[string]any{
		"path": "package.json", "content": "{\"name\":\"e2e-tools-test\",\"version\":\"0.0.1\"}\n", "message": "e2e: package.json for npm publish"})
	run(ag, extRepo, "read", 30*time.Second, map[string]any{"path": "e2e-tools.txt", "ref": "team:demo:main"})
	run(ag, extRepo, "grep", 30*time.Second, map[string]any{"pattern": "line-two", "ref": "team:demo:main"})
	run(ag, extRepo, "edit", 60*time.Second, map[string]any{
		"path": "e2e-tools.txt", "start-line": 1, "end-line": 1, "content": "EDITED-one", "message": "e2e edit"})
	logC, _ := run(ag, extRepo, "vcs-log", 30*time.Second, map[string]any{"rev": "team:demo:main", "limit": 6})
	run(ag, extRepo, "vcs-graph", 30*time.Second, map[string]any{"ref": "team:demo:main", "limit": 10})
	run(ag, extRepo, "vcs-show", 30*time.Second, map[string]any{"rev": "main"})
	shortRe := regexp.MustCompile(`\b[0-9a-f]{8}\b`)
	ids := shortRe.FindAllString(logC, -1)
	if len(ids) >= 2 {
		run(ag, extRepo, "vcs-diff", 30*time.Second, map[string]any{"rev-a": ids[1], "rev-b": ids[0]})
	} else {
		run(ag, extRepo, "vcs-diff", 30*time.Second, map[string]any{"rev-a": "main", "rev-b": "e2e-rb4"})
	}
	run(ag, extRepo, "vcs-blame", 30*time.Second, map[string]any{"rev": "main", "path": "e2e-tools.txt"})
	run(ag, extRepo, "vcs-rebase", 60*time.Second, map[string]any{"source": "e2e-rb4"})
	run(ag, extRepo, "vcs-resolve", 60*time.Second, map[string]any{"path": "e2e-tools.txt", "content": "RESOLVED-by-e2e\n"})
	run(ag, extRepo, "write", 60*time.Second, map[string]any{"path": "e2e-tmp-del.txt", "content": "bye\n", "message": "e2e temp"})
	run(ag, extRepo, "delete", 60*time.Second, map[string]any{"path": "e2e-tmp-del.txt", "message": "e2e delete"})

	fmt.Println("\n===== repo management (vcs-* tools) =====")
	run(ag, extRepo, "vcs-tag-list", 30*time.Second, map[string]any{})
	run(ag, extRepo, "vcs-tag-set", 30*time.Second, map[string]any{"name": "e2e-tag"})
	_, mrData := run(ag, extRepo, "vcs-mr-create", 30*time.Second, map[string]any{"title": "e2e MR", "target": "main"})
	run(ag, extRepo, "vcs-mr-list", 30*time.Second, map[string]any{})
	mrIID := dataStr(mrData, "iid")
	if mrIID == "" {
		mrIID = "1"
	}
	run(ag, extRepo, "vcs-mr-comment", 30*time.Second, map[string]any{"iid": mrIID, "body": "e2e comment"})
	run(ag, extRepo, "vcs-mr-merge", 30*time.Second, map[string]any{"iid": mrIID})

	fmt.Println("\n===== ops extension =====")
	run(ag, extOps, "container-search", 30*time.Second, map[string]any{})
	run(ag, extOps, "container-search", 30*time.Second, map[string]any{"repo": "demo"})
	run(ag, extOps, "package-search", 30*time.Second, map[string]any{"protocol": "npm"})
	run(ag, extOps, "sandbox-create", 600*time.Second, map[string]any{
		"image": "docker.io/library/golang:1.26-alpine"})
	run(ag, extOps, "sandbox-write", 60*time.Second, map[string]any{"path": "w.txt", "content": "hello sandbox\n"})
	run(ag, extOps, "sandbox-read", 30*time.Second, map[string]any{"path": "w.txt"})
	run(ag, extOps, "sandbox-edit", 60*time.Second, map[string]any{
		"path": "w.txt", "start-line": 1, "end-line": 1, "content": "hello sandbox EDITED"})
	_, data := run(ag, extOps, "sandbox-run", 120*time.Second, map[string]any{
		"command": "cat w.txt", "timeout-ms": 15000})
	jobID := dataStr(data, "job-id")
	run(ag, extOps, "sandbox-job-list", 30*time.Second, map[string]any{})
	if jobID != "" {
		run(ag, extOps, "sandbox-job-output", 30*time.Second, map[string]any{"job-id": jobID})
		run(ag, extOps, "sandbox-job-wait", 30*time.Second, map[string]any{"job-id": jobID, "timeout-ms": 1000})
	} else {
		fmt.Println("NOTE no job-id from sandbox-run; job tools run standalone")
		run(ag, extOps, "sandbox-job-list", 30*time.Second, map[string]any{})
	}
	// background job (sleep) for stdin/kill: sandbox-run returns after the
	// short JobWait window while the worker keeps the job registered.
	_, bgData := run(ag, extOps, "sandbox-run", 120*time.Second, map[string]any{
		"command": "sleep 300", "timeout-ms": 2000})
	bgJob := dataStr(bgData, "job-id")
	if bgJob != "" {
		run(ag, extOps, "sandbox-job-stdin", 30*time.Second, map[string]any{"job-id": bgJob, "data": "ignored-by-sleep\n"})
		run(ag, extOps, "sandbox-job-kill", 30*time.Second, map[string]any{"job-id": bgJob})
	} else {
		runExpectErr(ag, extOps, "sandbox-job-stdin", 30*time.Second, map[string]any{"job-id": "none", "data": "x"})
		runExpectErr(ag, extOps, "sandbox-job-kill", 30*time.Second, map[string]any{"job-id": "none"})
	}
	portC, _ := run(ag, extOps, "sandbox-port", 120*time.Second, map[string]any{
		"sandbox-path": "w.txt", "repo-path": "ported-from-sandbox.txt", "message": "e2e port"})
	_ = portC
	run(ag, extRepo, "read", 30*time.Second, map[string]any{"path": "ported-from-sandbox.txt", "ref": "team:demo:main"})
	if code := os.Getenv("AGENT_FILE_CODE"); code != "" {
		run(ag, extOps, "sandbox-download", 60*time.Second, map[string]any{"code": code, "path": "downloaded.bin"})
		run(ag, extOps, "sandbox-read", 30*time.Second, map[string]any{"path": "downloaded.bin"})
	} else {
		fmt.Println("NOTE sandbox-download skipped (AGENT_FILE_CODE unset; upload a file via agent IngestFile first)")
	}
	buildC, _ := run(ag, extOps, "ci-run", 120*time.Second, map[string]any{
		"preset": "container-build", "dockerfile-path": "Dockerfile", "tag": "e2e-tools"})
	runID := ""
	if m := regexp.MustCompile(`run ([^,\s]+)`).FindStringSubmatch(buildC); m != nil {
		runID = m[1]
	}
	run(ag, extOps, "ci-run", 120*time.Second, map[string]any{"preset": "npm-publish", "protocol": "npm"})
	run(ag, extOps, "ci-run", 120*time.Second, map[string]any{"workflow": ""})
	run(ag, extOps, "service-deploy", 600*time.Second, map[string]any{
		"image": "docker.io/library/nginx:alpine", "name": "e2e-tools-svc"})
	run(ag, extOps, "service-list", 30*time.Second, map[string]any{})
	run(ag, extOps, "service-list", 30*time.Second, map[string]any{"all": true})
	run(ag, extOps, "pull-git-repo", 300*time.Second, map[string]any{
		"git-url": "http://forgejo.develop.10.199.64.20.nip.io/easylab/team/demo.git", "org": "e2etest"})


	fmt.Println("\n===== rbac (HTTP through the real gateway) =====")
	runRBAC()

	fmt.Println("\n===== summary =====")
	fmt.Printf("pass=%d expected-error=%d fail=%d\n", pass, expErr, fail)
	var missed []string
	for ext, tools := range discovered {
		if ext == "bundled" {
			continue // abcp-sdk agent surface, out of scope for easylab
		}
		for _, t := range tools {
			if !covered[ext+"/"+t] {
				missed = append(missed, ext+"/"+t)
			}
		}
	}
	if len(missed) > 0 {
		sort.Strings(missed)
		fmt.Println("NOT COVERED:", strings.Join(missed, ", "))
	}
	if runID != "" {
		fmt.Println("BUILD_RUN_ID=" + runID)
	}
	if fail > 0 {
		os.Exit(1)
	}
}
