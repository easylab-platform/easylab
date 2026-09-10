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
)

func (s *server) registerBuildTools(m map[string]extension.ToolSpec) {

	m["container-build"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			ws, _, err := s.resolveWorkspace(ctx, args, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			image := strArg(args, "tag")
			if image == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "container-build: missing 'tag' (image name)", "container-build：缺少 'tag'（镜像名）")
			}
			// Tag defaults to the session branch (matching container-build's
			// historical {tag}:{branch}); an explicit image_tag overrides it.
			ref := image + ":" + ws.branchOrDefault()
			if imageTag := strArg(args, "image-tag"); imageTag != "" {
				ref = image + ":" + imageTag
			}
			fullImage := s.artifactImageHost + "/" + ref
			// Build via easylab WorkflowService produce.oci-build (image build +
			// push). The workflow is created (single job, no needs) and run.
			wfRes, err := s.sdk.Workflow.CreateWorkflow(ctx, connect.NewRequest(&easylabv1.CreateWorkflowRequest{Workflow: &easylabv1.Workflow{
				Name: "container-build-" + ws.branchOrDefault() + "-" + shortID(fmt.Sprintf("%d", time.Now().UnixNano())),
				Org:  ws.org, Repo: ws.repo, Branch: ws.branchOrDefault(),
				On: &easylabv1.Trigger{Events: []string{"manual"}},
				Jobs: []*easylabv1.JobDef{{
					Id:      "build",
					RunsOn:  []string{"os=linux", "is_container=true"},
					Steps:   []*easylabv1.Step{},
					Produce: &easylabv1.Produce{Action: "oci-build", Tag: fullImage},
				}},
			}}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "container-build workflow failed: %v", "container-build 工作流失败：%v", err)
			}
			wid := wfRes.Msg.GetWorkflow().GetId()
			runRes, err := s.sdk.Workflow.TriggerRun(ctx, connect.NewRequest(&easylabv1.TriggerRunRequest{WorkflowId: wid}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "container-build trigger failed: %v", "container-build 触发失败：%v", err)
			}
			out := fmt.Sprintf("Build launched (run %s, image %s).", runRes.Msg.GetRun().GetId(), fullImage)
			return extension.ToolResultData{Content: out}, nil
		},
	}
	m["container-search"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
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
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "container-search failed: %v", "container-search 失败：%v", err)
			}
			return extension.ToolResultData{Content: v}, nil
		},
	}
	m["package-publish"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			protocol := strArg(args, "protocol")
			org, repo, branch := strArg(args, "org"), strArg(args, "repo"), strArg(args, "branch")
			if org == "" || repo == "" {
				ws, _, err := s.resolveWorkspace(ctx, args, sessionName)
				if err != nil {
					return extension.ToolResultData{}, err
				}
				if org == "" {
					org, repo = ws.org, ws.repo
				}
				if branch == "" {
					branch = ws.branch
				}
			}
			// Publish via easylab WorkflowService produce.publish-protocol.
			wfRes, err := s.sdk.Workflow.CreateWorkflow(ctx, connect.NewRequest(&easylabv1.CreateWorkflowRequest{Workflow: &easylabv1.Workflow{
				Name: "publish-" + protocol + "-" + shortID(fmt.Sprintf("%d", time.Now().UnixNano())),
				Org:  org, Repo: repo, Branch: branchOrDefault(branch),
				On: &easylabv1.Trigger{Events: []string{"manual"}},
				Jobs: []*easylabv1.JobDef{{
					Id:     "publish",
					RunsOn: []string{"os=linux", "is_container=true"},
					Steps:  []*easylabv1.Step{{Name: "publish", Run: "true"}},
					Produce: &easylabv1.Produce{Action: "publish-protocol", Protocol: protocol,
						Name: strArg(args, "name"), Version: strArg(args, "version"), File: strArg(args, "file")},
				}},
			}}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "package-publish workflow failed: %v", "package-publish 工作流失败：%v", err)
			}
			wid := wfRes.Msg.GetWorkflow().GetId()
			_, err = s.sdk.Workflow.TriggerRun(ctx, connect.NewRequest(&easylabv1.TriggerRunRequest{WorkflowId: wid}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "package-publish trigger failed: %v", "package-publish 触发失败：%v", err)
			}
			ref := fmt.Sprintf("%s/%s@%s", org, repo, branchOrDefault(branch))
			content := fmt.Sprintf("Publish launched (%s, context %s).", protocol, ref)
			return extension.ToolResultData{Content: content}, nil
		},
	}
	m["package-search"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			typesRes, err := s.sdk.Registry.ListPackageTypes(ctx, connect.NewRequest(&easylabv1.ListPackageTypesRequest{}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "package-search failed: %v", "package-search 失败：%v", err)
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
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			gitURL := strArg(args, "git-url")
			if gitURL == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "pull-git-repo: missing 'git_url'", "pull-git-repo：缺少 'git_url'")
			}
			repo := inferRepoFromGitURL(gitURL)
			if repo == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "cannot infer repo name from %s", "无法从 %s 推导仓库名", gitURL)
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
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "create repo failed: %v", "创建仓库失败：%v", err)
				}
			}
			if _, err := s.sdk.Lab.SetMirror(ctx, connect.NewRequest(&easylabv1.SetMirrorRequest{
				Org: org, Repo: repo, PullUrl: gitURL,
			})); err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "set mirror failed: %v", "设置镜像失败：%v", err)
			}
			return extension.ToolResultData{Content: fmt.Sprintf("mirroring %s/%s from %s (pull scheduled)", org, repo, gitURL)}, nil
		},
	}
}
