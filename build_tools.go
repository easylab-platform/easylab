package main

import (
	"connectrpc.com/connect"
	"context"
	"fmt"
	"github.com/abcp-sdk/abc-protocol-go/extension"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"net/url"
	"strings"
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
			payload := map[string]interface{}{
				"org":      ws.org,
				"repo":     ws.repo,
				"branch":   ws.branch,
				"image":    fullImage,
				"export":   "push",
				"no-cache": boolArg(args, "no-cache"),
			}
			if df := strArg(args, "dockerfile-path"); df != "" {
				payload["dockerfile"] = df
			}
			id, err := s.opsSubmitBuild(ctx, payload)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "container-build failed: %v", "container-build 失败：%v", err)
			}
			out, err := s.awaitOpsTaskProgress(ctx, "build", fullImage, id, callID)
			return extension.ToolResultData{Content: out}, err
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
			res, err := s.publishPackage(ctx, protocol, org, repo, branch,
				strArg(args, "name"), strArg(args, "version"),
				strArg(args, "file"), strArg(args, "dockerfile-path"))
			return extension.ToolResultData{Content: res}, err
		},
	}
	m["package-search"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			types, err := s.sdk.ListPackageTypes(ctx)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "package-search failed: %v", "package-search 失败：%v", err)
			}
			proto := strArg(args, "protocol")
			var lines []string
			for _, t := range types {
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
			res, err := s.sdk.Lab.CloneRepo(ctx, connect.NewRequest(&easylabv1.CloneRepoRequest{
				Org: org, Repo: repo, GitUrl: gitURL,
			}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "clone failed: %v", "克隆失败：%v", err)
			}
			ok := "failed"
			if res.Msg.GetOk() {
				ok = "cloned"
			}
			return extension.ToolResultData{Content: ok + " " + org + "/" + repo}, nil
		},
	}
}
