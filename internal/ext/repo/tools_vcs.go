package repoext

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	abcprotocol "github.com/abcp-sdk/abc-protocol-go/v2"
	"github.com/abcp-sdk/abc-protocol-go/v2/extension"
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
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgMissingName)
			}
			target := abcprotocol.ArgString(args, "target")
			if _, err := s.sdk.Lab.SetTag(ctx, connect.NewRequest(&easylabv1.SetTagRequest{
				Org: o, Repo: r, Name: name, Target: target,
			})); err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lcf(ctx, s.ext, tenant, sessionName, MsgSetTagArg, name)}, nil
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
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgTitleAndTargetAreRequired)
			}
			res, err := s.sdk.Lab.CreateMergeRequest(ctx, connect.NewRequest(&easylabv1.CreateMergeRequestRequest{
				Org: o, Repo: r, Title: title, Description: abcprotocol.ArgString(args, "description"),
				Source: source, Target: target,
			}))
			if err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			iid := res.Msg.GetMergeRequest().GetIid()
			// Wake the session that owns the TARGET branch so the work does not
			// stall until someone polls: a subsession on its own branch opens the
			// MR and this notifies the default-branch session that created it.
			// Best-effort — a wake failure never fails the MR creation.
			s.notifyTargetSession(ctx, tenant, o, r, target, sessionName, iid, source)
			return extension.ToolResultData{Content: lcf(ctx, s.ext, tenant, sessionName, MsgOpenedChangeRequestArgArgArg, iid, source, target),
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
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgIidAndBodyAreRequired)
			}
			if _, err := s.sdk.Lab.AddComment(ctx, connect.NewRequest(&easylabv1.AddCommentRequest{
				Org: o, Repo: r, Iid: iid, Body: body, Path: abcprotocol.ArgString(args, "path"),
			})); err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lcf(ctx, s.ext, tenant, sessionName, MsgCommentedOnArg, iid)}, nil
		},
	}

	m["subsession-create"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID, sessionName, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			o, r, b, err := ownRepo(ctx, tenant, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			branch := abcprotocol.ArgString(args, "branch")
			prompt := abcprotocol.ArgString(args, "prompt")
			if branch == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgMissingBranchTheNewBranchNameChosenByY)
			}
			if prompt == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgMissingPromptTheSelfContainedTask)
			}
			if !validSessionComponent(branch) {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgInvalidBranchNameArgLettersDigitsOnlyNo, branch)
			}
			// Only the repository's DEFAULT-branch session may branch off —
			// this keeps the new branch anchored at the integration branch and
			// mirrors the merge rule (only the default-branch session merges).
			def, derr := s.repoDefaultBranch(ctx, o, r)
			if derr != nil {
				return extension.ToolResultData{}, errDownstream("easylab", derr)
			}
			if def == "" {
				def = "main"
			}
			if b != def {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSubsessionsMayOnlyBeStartedFromTheArgD, def, b)
			}
			// The new branch must not already exist, and no session may already
			// own it (the branch and its session are 1:1).
			tree, terr := s.lab.GetRepoTree(ctx)
			if terr != nil {
				return extension.ToolResultData{}, errDownstream("easylab", terr)
			}
			if tree.branchExists(o, r, branch) {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgBranchArgAlreadyExists, branch)
			}
			child := namingSession(o, r, branch)
			if sessions, lerr := s.ag.ListSessions(ctx); lerr == nil && sessions[child] {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgASessionForBranchArgAlreadyExists, branch)
			}
			// Create the branch off the current (default) branch, then the
			// child session bound to it with the host's fixed 'build' preset.
			// The gateway derives the generic `group` (= org/repo) from the
			// coordinates, so the new session is grouped with its siblings —
			// there is no parent/child relation.
			if err := s.lab.EnsureBranch(ctx, o, r, b, branch); err != nil {
				return extension.ToolResultData{}, err
			}
			if err := s.ag.CreateRepoSession(ctx, o, r, branch, "build"); err != nil {
				return extension.ToolResultData{}, errDownstream("agent", err)
			}
			// Materialize the session↔branch mapping immediately so the
			// child's first tool call never races the created-event.
			if err := s.bindRow(ctx, tenant, o, r, branch, child); err != nil {
				return extension.ToolResultData{}, errDownstream("postgres", err)
			}
			handoff := prompt + "\n\n" +
				"[subsession] When the work is done, open a change request (MR) from " +
				"this branch into '" + def + "' with the 'vcs-mr-create' tool " +
				"(source='" + branch + "', target='" + def + "'), then stop. That MR is " +
				"how the result is delivered."
			if _, perr := s.ag.Prompt(ctx, child, handoff); perr != nil {
				return extension.ToolResultData{}, errDownstream("agent", perr)
			}
			return extension.ToolResultData{
				Content: lcf(ctx, s.ext, tenant, sessionName, MsgStartedBranchArgWithSubsessionArgItIsW, branch, child, def),
				Data:    map[string]interface{}{"branch": branch, "session": child},
			}, nil
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
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgMergeIsOnlyAllowedFromTheArgDefaultBra, def, b)
			}
			iid := abcprotocol.ArgString(args, "iid")
			if iid == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgMissingIid)
			}
			res, err := s.sdk.Lab.MergeMergeRequest(ctx, connect.NewRequest(&easylabv1.MergeMergeRequestRequest{Org: o, Repo: r, Iid: iid}))
			if err != nil {
				return extension.ToolResultData{}, errDownstream("easylab", err)
			}
			return extension.ToolResultData{Content: lcf(ctx, s.ext, tenant, sessionName, MsgMergedArgRevisionArgArgConflictS, iid, shortID(res.Msg.GetRevisionId()), res.Msg.GetConflicts()),
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

// notifyTargetSession wakes the session that owns a merge request's TARGET
// branch with a user_prompt, so the work continues without polling. The target
// session is named "org:repo:<target>". Best-effort: any failure is logged and
// swallowed (the MR itself is already created).
func (s *server) notifyTargetSession(ctx context.Context, tenant, org, repo, target, source, iid, sourceBranch string) {
	if target == "" || target == sourceBranch {
		return
	}
	owner := namingSession(org, repo, target)
	text := fmt.Sprintf(
		"Subsession '%s' opened change request #%s (%s → %s). Review and merge it with the 'vcs-mr-merge' tool.",
		sourceBranch, iid, sourceBranch, target)
	if _, err := s.ag.Prompt(ctx, owner, text); err != nil {
		log.Warn("mr-create: notify target session failed",
			"target", owner, "iid", iid, "err", err)
	}
}
