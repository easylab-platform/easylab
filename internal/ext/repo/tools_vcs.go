package repoext

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"connectrpc.com/connect"

	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
	"github.com/abcp-sdk/abc-protocol-go/extension"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easylab/internal/ext"
)

// registerVCSTools binds the repository-management and collaboration tools.
// Every tool here resolves its target repo FROM THE SESSION ONLY (org/repo/
// branch derived from session_name) — a session may only touch its own repo.
// The sole management action is vcs-mr-merge, and it is allowed ONLY from the
// session whose branch is the repository's default branch.
func (s *server) registerVCSTools(m map[string]extension.ToolSpec) {
	// ownRepo resolves the session's own repo triple; it never accepts an
	// org/repo override.
	ownRepo := func(ctx context.Context, tenant, sessionName string) (string, string, string, error) {
		return s.sessionBase(ctx, tenant, map[string]interface{}{}, sessionName)
	}

	m["vcs-branch-list"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			res, err := s.sdk.Lab.Branches(ctx, connect.NewRequest(&easylabv1.BranchesRequest{Org: o, Repo: r}))
			if err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			var b strings.Builder
			for _, x := range res.Msg.GetBranches() {
				fmt.Fprintf(&b, "%s %s\n", shortID(x.GetSha()), x.GetName())
			}
			return extension.ToolResultData{Content: strings.TrimSpace(b.String()),
				Data: map[string]interface{}{"branches": len(res.Msg.GetBranches())}}, nil
		},
	}

	m["vcs-branch-create"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, b, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			name := abcprotocol.ArgString(args, "name")
			if name == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "missing 'name'", "缺少 'name'")
			}
			from := abcprotocol.ArgString(args, "from")
			if from == "" {
				from = b
			}
			if _, err := s.sdk.Lab.CreateBranch(ctx, connect.NewRequest(&easylabv1.CreateBranchRequest{
				Org: o, Repo: r, Branch: name, From: from,
			})); err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName,
				fmt.Sprintf("created branch '%s' from '%s'.", name, from),
				fmt.Sprintf("已从 '%s' 创建分支 '%s'。", from, name))}, nil
		},
	}

	m["vcs-branch-delete"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			name := abcprotocol.ArgString(args, "name")
			if name == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "missing 'name'", "缺少 'name'")
			}
			if _, err := s.sdk.Lab.DeleteBranch(ctx, connect.NewRequest(&easylabv1.DeleteBranchRequest{
				Org: o, Repo: r, Branch: name,
			})); err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName,
				fmt.Sprintf("deleted branch '%s'.", name), fmt.Sprintf("已删除分支 '%s'。", name))}, nil
		},
	}

	m["vcs-tag-list"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			res, err := s.sdk.Lab.Tags(ctx, connect.NewRequest(&easylabv1.TagsRequest{Org: o, Repo: r}))
			if err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			var b strings.Builder
			for _, x := range res.Msg.GetTags() {
				fmt.Fprintf(&b, "%s %s\n", shortID(x.GetTarget()), x.GetName())
			}
			return extension.ToolResultData{Content: strings.TrimSpace(b.String())}, nil
		},
	}

	m["vcs-tag-set"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			name := abcprotocol.ArgString(args, "name")
			if name == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "missing 'name'", "缺少 'name'")
			}
			target := abcprotocol.ArgString(args, "target")
			if _, err := s.sdk.Lab.SetTag(ctx, connect.NewRequest(&easylabv1.SetTagRequest{
				Org: o, Repo: r, Name: name, Target: target,
			})); err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName,
				fmt.Sprintf("set tag '%s'.", name), fmt.Sprintf("已创建标签 '%s'。", name))}, nil
		},
	}

	m["vcs-tag-delete"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			name := abcprotocol.ArgString(args, "name")
			if name == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "missing 'name'", "缺少 'name'")
			}
			if _, err := s.sdk.Lab.DeleteTag(ctx, connect.NewRequest(&easylabv1.DeleteTagRequest{
				Org: o, Repo: r, Name: name,
			})); err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName,
				fmt.Sprintf("deleted tag '%s'.", name), fmt.Sprintf("已删除标签 '%s'。", name))}, nil
		},
	}

	// ---- merge requests (own repo only) ----

	m["vcs-mr-create"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			title := abcprotocol.ArgString(args, "title")
			source := abcprotocol.ArgString(args, "source")
			target := abcprotocol.ArgString(args, "target")
			if title == "" || source == "" || target == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "title, source and target are required", "title、source、target 均为必填")
			}
			res, err := s.sdk.Lab.CreateMergeRequest(ctx, connect.NewRequest(&easylabv1.CreateMergeRequestRequest{
				Org: o, Repo: r, Title: title, Description: abcprotocol.ArgString(args, "description"),
				Source: source, Target: target,
			}))
			if err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			iid := res.Msg.GetMergeRequest().GetIid()
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName,
				fmt.Sprintf("opened change request #%s (%s → %s).", iid, source, target),
				fmt.Sprintf("已创建合并请求 #%s（%s → %s）。", iid, source, target)),
				Data: map[string]interface{}{"iid": iid}}, nil
		},
	}

	m["vcs-mr-list"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			res, err := s.sdk.Lab.ListMergeRequests(ctx, connect.NewRequest(&easylabv1.ListMergeRequestsRequest{
				Org: o, Repo: r, State: abcprotocol.ArgString(args, "state"),
			}))
			if err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			var b strings.Builder
			for _, mr := range res.Msg.GetMergeRequests() {
				fmt.Fprintf(&b, "#%s [%s] %s (%s → %s)\n", mr.GetIid(), mr.GetState(), mr.GetTitle(), mr.GetSource(), mr.GetTarget())
			}
			return extension.ToolResultData{Content: strings.TrimSpace(b.String())}, nil
		},
	}

	m["vcs-mr-get"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			iid := abcprotocol.ArgString(args, "iid")
			res, err := s.sdk.Lab.GetMergeRequest(ctx, connect.NewRequest(&easylabv1.GetMergeRequestRequest{Org: o, Repo: r, Iid: iid}))
			if err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			mr := res.Msg.GetMergeRequest()
			return extension.ToolResultData{Content: fmt.Sprintf("#%s [%s] %s\n%s\n%s → %s",
				mr.GetIid(), mr.GetState(), mr.GetTitle(), mr.GetDescription(), mr.GetSource(), mr.GetTarget())}, nil
		},
	}

	m["vcs-mr-review"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			iid := abcprotocol.ArgString(args, "iid")
			state := abcprotocol.ArgString(args, "state")
			if iid == "" || state == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "iid and state are required", "iid 与 state 为必填")
			}
			if _, err := s.sdk.Lab.AddReview(ctx, connect.NewRequest(&easylabv1.AddReviewRequest{
				Org: o, Repo: r, Iid: iid, State: state, Body: abcprotocol.ArgString(args, "body"),
			})); err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName,
				fmt.Sprintf("reviewed #%s (%s).", iid, state), fmt.Sprintf("已评审 #%s（%s）。", iid, state))}, nil
		},
	}

	m["vcs-mr-review-list"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			res, err := s.sdk.Lab.ListReviews(ctx, connect.NewRequest(&easylabv1.ListReviewsRequest{Org: o, Repo: r, Iid: abcprotocol.ArgString(args, "iid")}))
			if err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			var b strings.Builder
			for _, rv := range res.Msg.GetReviews() {
				fmt.Fprintf(&b, "%s %s: %s\n", rv.GetReviewer(), rv.GetState(), rv.GetBody())
			}
			return extension.ToolResultData{Content: strings.TrimSpace(b.String())}, nil
		},
	}

	m["vcs-mr-comment"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			iid, body := abcprotocol.ArgString(args, "iid"), abcprotocol.ArgString(args, "body")
			if iid == "" || body == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "iid and body are required", "iid 与 body 为必填")
			}
			if _, err := s.sdk.Lab.AddComment(ctx, connect.NewRequest(&easylabv1.AddCommentRequest{
				Org: o, Repo: r, Iid: iid, Body: body, Path: abcprotocol.ArgString(args, "path"),
			})); err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName,
				fmt.Sprintf("commented on #%s.", iid), fmt.Sprintf("已在 #%s 评论。", iid))}, nil
		},
	}

	m["vcs-mr-comment-list"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			res, err := s.sdk.Lab.ListComments(ctx, connect.NewRequest(&easylabv1.ListCommentsRequest{Org: o, Repo: r, Iid: abcprotocol.ArgString(args, "iid")}))
			if err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			var b strings.Builder
			for _, cm := range res.Msg.GetComments() {
				fmt.Fprintf(&b, "%s: %s\n", cm.GetAuthor(), cm.GetBody())
			}
			return extension.ToolResultData{Content: strings.TrimSpace(b.String())}, nil
		},
	}

	// vcs-mr-merge — the ONLY management action exposed to the agent, and only
	// callable from the session whose branch IS the repository's default branch.
	m["vcs-mr-merge"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, b, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			def, derr := s.repoDefaultBranch(ctx, o, r)
			if derr != nil {
				return extension.ToolResultData{}, errDownstream("easylab", derr)
			}
			if def == "" {
				def = "main"
			}
			if b != def {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName,
					"merge is only allowed from the '%s' (default-branch) session (this session is on '%s')", "仅允许 '%s'（默认分支）会话执行合并（当前会话在 '%s'）", def, b)
			}
			iid := abcprotocol.ArgString(args, "iid")
			if iid == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "missing 'iid'", "缺少 'iid'")
			}
			res, err := s.sdk.Lab.MergeMergeRequest(ctx, connect.NewRequest(&easylabv1.MergeMergeRequestRequest{Org: o, Repo: r, Iid: iid}))
			if err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName,
				fmt.Sprintf("merged #%s (revision %s, %d conflict(s)).", iid, shortID(res.Msg.GetRevisionId()), res.Msg.GetConflicts()),
				fmt.Sprintf("已合并 #%s（revision %s，%d 个冲突）。", iid, shortID(res.Msg.GetRevisionId()), res.Msg.GetConflicts())),
				Data: map[string]interface{}{"revision_id": res.Msg.GetRevisionId(), "conflicts": res.Msg.GetConflicts()}}, nil
		},
	}

	// ---- repo settings ----

	m["vcs-repo-config"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			req := &easylabv1.UpdateRepoRequest{Org: o, Repo: r}
			if v := abcprotocol.ArgString(args, "visibility"); v != "" {
				req.Visibility = &v
			}
			if v := abcprotocol.ArgString(args, "description"); v != "" {
				req.Description = &v
			}
			if v := abcprotocol.ArgString(args, "default-branch"); v != "" {
				req.DefaultBranch = &v
			}
			if _, err := s.sdk.Lab.UpdateRepo(ctx, connect.NewRequest(req)); err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName, "repository settings updated.", "仓库设置已更新。")}, nil
		},
	}

	// ---- releases ----

	m["vcs-release-create"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			tag := abcprotocol.ArgString(args, "tag")
			if tag == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "missing 'tag'", "缺少 'tag'")
			}
			if _, err := s.sdk.Lab.CreateRelease(ctx, connect.NewRequest(&easylabv1.CreateReleaseRequest{
				Org: o, Repo: r, Tag: tag, Name: abcprotocol.ArgString(args, "name"),
				Description: abcprotocol.ArgString(args, "description"), Target: abcprotocol.ArgString(args, "target"),
				Draft: abcprotocol.ArgBool(args, "draft", false), Prerelease: abcprotocol.ArgBool(args, "prerelease", false),
			})); err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName,
				fmt.Sprintf("created release '%s'.", tag), fmt.Sprintf("已创建 release '%s'。", tag))}, nil
		},
	}

	m["vcs-release-list"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			res, err := s.sdk.Lab.ListReleases(ctx, connect.NewRequest(&easylabv1.ListReleasesRequest{Org: o, Repo: r}))
			if err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			var b strings.Builder
			for _, rel := range res.Msg.GetReleases() {
				fmt.Fprintf(&b, "%s %s (%d asset(s))\n", rel.GetTag(), rel.GetName(), len(rel.GetAssets()))
			}
			return extension.ToolResultData{Content: strings.TrimSpace(b.String())}, nil
		},
	}

	m["vcs-release-delete"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			tag := abcprotocol.ArgString(args, "tag")
			if _, err := s.sdk.Lab.DeleteRelease(ctx, connect.NewRequest(&easylabv1.DeleteReleaseRequest{Org: o, Repo: r, Tag: tag})); err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName,
				fmt.Sprintf("deleted release '%s'.", tag), fmt.Sprintf("已删除 release '%s'。", tag))}, nil
		},
	}

	m["vcs-release-upload"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			tag := abcprotocol.ArgString(args, "tag")
			name := abcprotocol.ArgString(args, "name")
			if tag == "" || name == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "tag and name are required", "tag 与 name 为必填")
			}
			data := []byte(abcprotocol.ArgString(args, "content"))
			if _, err := s.sdk.Lab.UploadReleaseAsset(ctx, connect.NewRequest(&easylabv1.UploadReleaseAssetRequest{
				Org: o, Repo: r, Tag: tag, Name: name, Data: data, ContentType: abcprotocol.ArgString(args, "content-type"),
			})); err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName,
				fmt.Sprintf("uploaded asset '%s' to release '%s' (%d bytes).", name, tag, len(data)),
				fmt.Sprintf("已上传资产 '%s' 到 release '%s'（%d 字节）。", name, tag, len(data)))}, nil
		},
	}

	// vcs-release-download writes the asset into the session sandbox workspace
	// (large bytes never travel inline through the tool result) and returns the
	// sandbox path + metadata.
	m["vcs-release-download"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			o, r, _, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			tag, name := abcprotocol.ArgString(args, "tag"), abcprotocol.ArgString(args, "name")
			if tag == "" || name == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "tag and name are required", "tag 与 name 为必填")
			}
			res, err := s.sdk.Lab.DownloadReleaseAsset(ctx, connect.NewRequest(&easylabv1.DownloadReleaseAssetRequest{
				Org: o, Repo: r, Tag: tag, Name: name,
			}))
			if err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			// Deliver to the shared /data downloads dir (the hostPath mount the
			// build/job pods also see) and return the PATH + metadata — never the
			// bytes inline (release assets can be large).
			path, werr := s.writeDownload(name, res.Msg.GetData())
			if werr != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "write download failed: %v", "写入下载文件失败：%v", werr)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName,
				fmt.Sprintf("downloaded '%s' from release '%s' → %s (%d bytes).", name, tag, path, len(res.Msg.GetData())),
				fmt.Sprintf("已从 release '%s' 下载 '%s' → %s（%d 字节）。", tag, name, path, len(res.Msg.GetData()))),
				Data: map[string]interface{}{"path": path, "bytes": len(res.Msg.GetData())}}, nil
		},
	}

	// ---- package visibility ----

	m["vcs-package-visibility"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = withTenant(ctx, tenant)
			typ := abcprotocol.ArgString(args, "type")
			name := abcprotocol.ArgString(args, "name")
			vis := abcprotocol.ArgString(args, "visibility")
			if typ == "" || name == "" || vis == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "type, name and visibility are required", "type、name 与 visibility 为必填")
			}
			if _, err := s.sdk.Registry.SetPackageVisibility(ctx, connect.NewRequest(&easylabv1.SetPackageVisibilityRequest{
				Type: typ, Name: name, Visibility: vis,
			})); err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, tenant, sessionName,
				fmt.Sprintf("set %s/%s visibility to %s.", typ, name, vis),
				fmt.Sprintf("已将 %s/%s 的可见性设为 %s。", typ, name, vis))}, nil
		},
	}
}

// repoDefaultBranch resolves the repository's default branch via ListRepos.
func (s *server) repoDefaultBranch(ctx context.Context, org, repo string) (string, error) {
	res, err := s.sdk.Lab.ListRepos(ctx, connect.NewRequest(&easylabv1.ListReposRequest{}))
	if err != nil {
		return "", err
	}
	for _, r := range res.Msg.GetRepos() {
		if r.GetNamespace() == org && r.GetName() == repo {
			if db := r.GetDefaultBranch(); db != "" {
				return db, nil
			}
		}
	}
	return "", nil
}

// writeDownload writes release/asset bytes into the shared /data downloads
// directory (the hostPath mount the build + job pods also see) and returns the
// absolute path. Large bytes never travel inline through the tool result.
func (s *server) writeDownload(name string, data []byte) (string, error) {
	dir := filepath.Join(homeDir(), "downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	safe := strings.ReplaceAll(strings.ReplaceAll(name, "/", "_"), "..", "_")
	if safe == "" {
		safe = fmt.Sprintf("download-%d", time.Now().UnixNano())
	}
	path := filepath.Join(dir, safe)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// withTenant tags the context with the caller's lab tenant (X-Agent-Tenant).
func withTenant(ctx context.Context, tenant string) context.Context {
	return ext.WithLabTenant(ctx, tenant)
}
