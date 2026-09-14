package repoext

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
	"github.com/abcp-sdk/abc-protocol-go/extension"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easylab/internal/ext"
)

// registerVCSTools binds the repository-delivery tools an agent needs while
// working on ONE branch: list/create tags, and the change-request flow
// (create/list/comment/merge). Every tool resolves its target repo FROM THE
// SESSION ONLY (org/repo/branch from session_name) — a session may only touch
// its own repo. Administrative surfaces (repo visibility, package visibility,
// collaborators, branch CRUD, fork, releases) are intentionally NOT exposed:
// they belong to the human UI, not to the agent.
//
// vcs-mr-merge is the sole management action and is allowed ONLY from the
// session whose branch IS the repository's default branch.
func (s *server) registerVCSTools(m map[string]extension.ToolSpec) {
	// ownRepo resolves the session's own repo triple; it never accepts an
	// org/repo override.
	ownRepo := func(ctx context.Context, tenant, sessionName string) (string, string, string, error) {
		return s.sessionBase(ctx, tenant, map[string]interface{}{}, sessionName)
	}

	m["vcs-tag-list"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
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
			ctx = ext.WithLabTenant(ctx, tenant)
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

	m["vcs-mr-create"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			o, r, b, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			title := abcprotocol.ArgString(args, "title")
			target := abcprotocol.ArgString(args, "target")
			source := abcprotocol.ArgString(args, "source")
			if source == "" {
				source = b
			}
			if title == "" || target == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, "title and target are required", "title 与 target 为必填")
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
			ctx = ext.WithLabTenant(ctx, tenant)
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

	m["vcs-mr-comment"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
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

	// vcs-mr-merge — the ONLY management action exposed to the agent, and only
	// callable from the session whose branch IS the repository's default branch.
	m["vcs-mr-merge"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
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
