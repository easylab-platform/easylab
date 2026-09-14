package opsext

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/abcp-sdk/abc-protocol-go/extension"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easylab/internal/ext"
	"github.com/easylab-platform/easylab/internal/registry"
)

// registerBuildTools registers the CI/registry tools. Every build/publish/CI
// entry point is the single `ci-run` tool, which drives the one CI pipeline
// (WorkflowService -> K8sBackend): either a preset (`container-build`,
// `<protocol>-publish`) or a workflow declared in `.easylab/workflows.yaml`.
func (s *server) registerBuildTools(m map[string]extension.ToolSpec) {

	m["ci-run"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			preset := strArg(args, "preset")
			workflow := strArg(args, "workflow")
			org, repo, branch := strArg(args, "org"), strArg(args, "repo"), strArg(args, "branch")
			if org == "" || repo == "" || branch == "" {
				ws, _, err := s.resolveWorkspace(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				if org == "" {
					org = ws.org
				}
				if repo == "" {
					repo = ws.repo
				}
				if branch == "" {
					branch = ws.branch
				}
			}
			branch = branchOrDefault(branch)

			if preset != "" {
				return s.runPreset(ctx, args, tenant, sessionName, preset, org, repo, branch)
			}
			return s.runWorkflowFile(ctx, args, tenant, sessionName, workflow, org, repo, branch)
		},
	}

	m["container-search"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			// Search the OCI image registry. Default: all cached/published
			// images (incl. library/* base images like go:alpine). Optional
			// repo=<org/repo> filters to images built from that source repo;
			// source=push|pull filters origin; all=true returns everything.
			repo := strArg(args, "repo")
			src := strArg(args, "source")
			q := url.Values{}
			if repo != "" {
				q.Set("repo", repo)
			}
			if src != "" {
				q.Set("source", src)
			}
			all := boolArg(args, "all")
			if all {
				q.Set("all", "1")
			}
			// OCI catalog via the /v2 registry (h1 artifact face).
			u := s.artifact + "/v2/_catalog"
			if repo := strArg(args, "repo"); repo != "" {
				u += "?repo=" + url.QueryEscape(repo)
			}
			v, err := s.httpGetJSONArtifact(ctx, u)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "container-search failed: %v", "container-search 失败：%v", err)
			}
			return extension.ToolResultData{Content: v}, nil
		},
	}
	m["package-search"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			typesRes, err := s.sdk.Registry.ListPackageTypes(ctx, connect.NewRequest(&easylabv1.ListPackageTypesRequest{}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "package-search failed: %v", "package-search 失败：%v", err)
			}
			proto := strArg(args, "protocol")
			var lines []string
			for _, t := range typesRes.Msg.GetPackages() {
				if proto != "" && t.GetType() != proto {
					continue
				}
				lines = append(lines, t.GetType()+": "+fmt.Sprintf("%d packages", t.GetPackages()))
			}
			v := strings.Join(lines, "\n")
			if v == "" {
				v = "no packages found"
			}
			return extension.ToolResultData{Content: v}, nil
		},
	}
	m["pull-git-repo"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			gitURL := strArg(args, "git-url")
			if gitURL == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "pull-git-repo: missing 'git_url'", "pull-git-repo：缺少 'git_url'")
			}
			repo := inferRepoFromGitURL(gitURL)
			if repo == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "cannot infer repo name from %s", "无法从 %s 推导仓库名", gitURL)
			}
			org := strArg(args, "org")
			if org == "" {
				org = "external"
			}
			// CloneRepo RPC is a stub upstream; the supported import path is
			// EnsureRepo + SetMirror(pull URL) — the gateway's mirror loop
			// fetches on schedule (EASYVCS_MIRROR_TICK, default 5s).
			if _, err := s.sdk.Lab.EnsureRepo(ctx, connect.NewRequest(&easylabv1.EnsureRepoRequest{Org: org, Repo: repo})); err != nil {
				if connect.CodeOf(err) != connect.CodeAlreadyExists && connect.CodeOf(err) != connect.CodeInvalidArgument {
					return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "create repo failed: %v", "创建仓库失败：%v", err)
				}
			}
			if _, err := s.sdk.Lab.SetMirror(ctx, connect.NewRequest(&easylabv1.SetMirrorRequest{
				Org: org, Repo: repo, PullUrl: gitURL,
			})); err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "set mirror failed: %v", "设置镜像失败：%v", err)
			}
			return extension.ToolResultData{Content: fmt.Sprintf("mirroring %s/%s from %s (pull scheduled)", org, repo, gitURL)}, nil
		},
	}
}

// runPreset creates and triggers a one-job workflow from a built-in produce
// preset: `container-build` (oci-build) or `<protocol>-publish`
// (publish-protocol).
func (s *server) runPreset(ctx context.Context, args map[string]interface{}, tenant, sessionName, preset, org, repo, branch string) (extension.ToolResultData, error) {
	job := &easylabv1.JobDef{Id: "run", RunsOn: []string{"os=linux", "is_container=true"}}
	switch {
	case preset == "container-build":
		image := strArg(args, "tag")
		if image == "" {
			return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "ci-run: preset container-build requires 'tag'", "ci-run：预设 container-build 需要 'tag'")
		}
		ref := image + ":" + branch
		if imageTag := strArg(args, "image-tag"); imageTag != "" {
			ref = image + ":" + imageTag
		}
		job.Produce = &easylabv1.Produce{
			Action:     "oci-build",
			Tag:        registry.Join(s.artifactImageHost, ref),
			Dockerfile: strArg(args, "dockerfile-path"),
			Context:    strArg(args, "context"),
		}
	case strings.HasSuffix(preset, "-publish"):
		protocol := strArg(args, "protocol")
		if protocol == "" {
			protocol = strings.TrimSuffix(preset, "-publish")
		}
		job.Produce = &easylabv1.Produce{
			Action:   "publish-protocol",
			Protocol: protocol,
			Name:     strArg(args, "name"),
			Version:  strArg(args, "version"),
			File:     strArg(args, "file"),
		}
	default:
		return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "ci-run: unknown preset %q (container-build | <protocol>-publish)", "ci-run：未知预设 %q（container-build | <protocol>-publish）", preset)
	}
	wfRes, err := s.sdk.Workflow.CreateWorkflow(ctx, connect.NewRequest(&easylabv1.CreateWorkflowRequest{Workflow: &easylabv1.Workflow{
		Name: preset + "-" + shortID(fmt.Sprintf("%d", time.Now().UnixNano())),
		Org:  org, Repo: repo, Branch: branch,
		On:   &easylabv1.Trigger{Events: []string{"manual"}},
		Jobs: []*easylabv1.JobDef{job},
	}}))
	if err != nil {
		return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "ci-run workflow failed: %v", "ci-run 工作流失败：%v", err)
	}
	wid := wfRes.Msg.GetWorkflow().GetId()
	runRes, err := s.sdk.Workflow.TriggerRun(ctx, connect.NewRequest(&easylabv1.TriggerRunRequest{WorkflowId: wid}))
	if err != nil {
		return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "ci-run trigger failed: %v", "ci-run 触发失败：%v", err)
	}
	rid := runRes.Msg.GetRun().GetId()
	detail := ""
	if job.Produce != nil && job.Produce.Tag != "" {
		detail = ", image " + job.Produce.Tag
	}
	return extension.ToolResultData{
		Content: fmt.Sprintf("CI launched (preset %s, run %s%s).", preset, rid, detail),
		Data:    map[string]interface{}{"run_id": rid, "workflow_id": wid, "preset": preset},
	}, nil
}

// runWorkflowFile runs the workflow(s) declared in the branch's
// .easylab/workflows.yaml (name empty = all).
func (s *server) runWorkflowFile(ctx context.Context, args map[string]interface{}, tenant, sessionName, name, org, repo, branch string) (extension.ToolResultData, error) {
	presetArgs := map[string]string{}
	for arg, key := range map[string]string{"name_arg": "name", "version_arg": "version", "file_arg": "file"} {
		if v := strArg(args, arg); v != "" {
			presetArgs[key] = v
		}
	}
	res, err := s.sdk.Workflow.RunWorkflowFile(ctx, connect.NewRequest(&easylabv1.RunWorkflowFileRequest{
		Org: org, Repo: repo, Branch: branch, Name: name, Args: presetArgs,
	}))
	if err != nil {
		return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "ci-run failed: %v", "ci-run 失败：%v", err)
	}
	if e := res.Msg.GetError(); e != "" {
		return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "ci-run: %s", "ci-run：%s", e)
	}
	var ids []string
	for _, r := range res.Msg.GetRuns() {
		ids = append(ids, r.GetId())
	}
	content := fmt.Sprintf("Launched %d run(s) from .easylab/workflows.yaml: %s", len(ids), strings.Join(ids, ", "))
	if sk := res.Msg.GetSkipped(); len(sk) > 0 {
		content += fmt.Sprintf(" (skipped: %s)", strings.Join(sk, ", "))
	}
	return extension.ToolResultData{Content: content, Data: map[string]interface{}{"run_ids": ids}}, nil
}
