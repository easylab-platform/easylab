package repoext

import (
	"context"
	"fmt"

	"github.com/abcp-sdk/abc-protocol-go/v2/extension"
	"github.com/abcp-sdk/abc-protocol-go/v2/i18n"
)

// MsgKey names a runtime message that reaches the model (tool content/error).
// The constants below are the strongly-typed key set for this extension; a
// mistyped key is a compile error. The catalog is an OPEN locale map (add a
// language by adding a column), resolved by the shared SDK i18n core.
type MsgKey = string

const (
	MsgFailedToReadFileArgNotFoundOrInaccessi  MsgKey = "failed_to_read_file_arg_not_found_or_inaccessi"
	MsgFileArgHasArgLinesOffsetArgIsPastThe    MsgKey = "file_arg_has_arg_lines_offset_arg_is_past_the_"
	MsgWroteFileArgChangeArg                   MsgKey = "wrote_file_arg_change_arg"
	MsgDeletedFileArgChangeArg                 MsgKey = "deleted_file_arg_change_arg"
	MsgEditedFileArgArgChangeArg               MsgKey = "edited_file_arg_arg_change_arg"
	MsgNoMatchesForArgInRevArg                 MsgKey = "no_matches_for_arg_in_rev_arg"
	MsgNoOrganizationsOrRepositories           MsgKey = "no_organizations_or_repositories"
	MsgNoCommitsInGraph                        MsgKey = "no_commits_in_graph"
	MsgNoDiffBetweenArgAndArgArg               MsgKey = "no_diff_between_arg_and_arg_arg"
	MsgDiffArgBetweenArgArgNArg                MsgKey = "diff_arg_between_arg_arg_n_arg"
	MsgRebasedArgOntoArgWithArgConflictSArg    MsgKey = "rebased_arg_onto_arg_with_arg_conflict_s_arg"
	MsgRebasedArgOntoArgTipArgChangeArg        MsgKey = "rebased_arg_onto_arg_tip_arg_change_arg"
	MsgResolvedArgTipArgChangeArg              MsgKey = "resolved_arg_tip_arg_change_arg"
	MsgNoCommits                               MsgKey = "no_commits"
	MsgChangeArgHasNoContentDiff               MsgKey = "change_arg_has_no_content_diff"
	MsgChangesOfArgNArg                        MsgKey = "changes_of_arg_n_arg"
	MsgMissingPathArgument                     MsgKey = "missing_path_argument"
	MsgReadArgArg                              MsgKey = "read_arg_arg"
	MsgFailedToWriteFileArg                    MsgKey = "failed_to_write_file_arg"
	MsgFailedToDeleteFileArg                   MsgKey = "failed_to_delete_file_arg"
	MsgReadArgBeforeEditArg                    MsgKey = "read_arg_before_edit_arg"
	MsgFailedToWriteEditedResultArg            MsgKey = "failed_to_write_edited_result_arg"
	MsgFailedToListDirectoryArg                MsgKey = "failed_to_list_directory_arg"
	MsgMissingPatternArgument                  MsgKey = "missing_pattern_argument"
	MsgSearchFailedArg                         MsgKey = "search_failed_arg"
	MsgFailedToBrowseStructureArg              MsgKey = "failed_to_browse_structure_arg"
	MsgFailedToGetGraphArg                     MsgKey = "failed_to_get_graph_arg"
	MsgRevAAndRevBAreRequired                  MsgKey = "rev_a_and_rev_b_are_required"
	MsgFailedToGetDiffArg                      MsgKey = "failed_to_get_diff_arg"
	MsgMissingSourceArgument                   MsgKey = "missing_source_argument"
	MsgRebaseFailedArg                         MsgKey = "rebase_failed_arg"
	MsgResolveFailedArg                        MsgKey = "resolve_failed_arg"
	MsgFailedToGetBlameArg                     MsgKey = "failed_to_get_blame_arg"
	MsgFailedToGetCommitHistoryArg             MsgKey = "failed_to_get_commit_history_arg"
	MsgMissingRevArgument                      MsgKey = "missing_rev_argument"
	MsgFailedToViewChangeArg                   MsgKey = "failed_to_view_change_arg"
	MsgSetTagArg                               MsgKey = "set_tag_arg"
	MsgOpenedChangeRequestArgArgArg            MsgKey = "opened_change_request_arg_arg_arg"
	MsgCommentedOnArg                          MsgKey = "commented_on_arg"
	MsgStartedBranchArgWithSubsessionArgItIsW  MsgKey = "started_branch_arg_with_subsession_arg_it_is_w"
	MsgMergedArgRevisionArgArgConflictS        MsgKey = "merged_arg_revision_arg_arg_conflict_s"
	MsgMissingName                             MsgKey = "missing_name"
	MsgTitleAndTargetAreRequired               MsgKey = "title_and_target_are_required"
	MsgIidAndBodyAreRequired                   MsgKey = "iid_and_body_are_required"
	MsgMissingBranchTheNewBranchNameChosenByY  MsgKey = "missing_branch_the_new_branch_name_chosen_by_y"
	MsgMissingPromptTheSelfContainedTask       MsgKey = "missing_prompt_the_self_contained_task"
	MsgInvalidBranchNameArgLettersDigitsOnlyNo MsgKey = "invalid_branch_name_arg_letters_digits_only_no"
	MsgSubsessionsMayOnlyBeStartedFromTheArgD  MsgKey = "subsessions_may_only_be_started_from_the_arg_d"
	MsgBranchArgAlreadyExists                  MsgKey = "branch_arg_already_exists"
	MsgASessionForBranchArgAlreadyExists       MsgKey = "a_session_for_branch_arg_already_exists"
	MsgMergeIsOnlyAllowedFromTheArgDefaultBra  MsgKey = "merge_is_only_allowed_from_the_arg_default_bra"
	MsgMissingIid                              MsgKey = "missing_iid"
)

// catalog holds this extension's en/zh templates. Extend with more locales by
// adding a column to each entry.
var catalog = i18n.Catalog{
	"failed_to_read_file_arg_not_found_or_inaccessi": {"en": "failed to read file '%s': not found or inaccessible", "zh": "读取文件 '%s' 失败：未找到或不可访问"},
	"file_arg_has_arg_lines_offset_arg_is_past_the_": {"en": "file '%s' has %d lines; offset=%d is past the end.", "zh": "文件 '%s' 共 %d 行；offset=%d 已超出末尾。"},
	"wrote_file_arg_change_arg":                      {"en": "wrote file '%s' (change %s)", "zh": "已写入文件 '%s'（变更 %s）"},
	"deleted_file_arg_change_arg":                    {"en": "deleted file '%s' (change %s)", "zh": "已删除文件 '%s'（变更 %s）"},
	"edited_file_arg_arg_change_arg":                 {"en": "edited file '%s': %s (change %s)", "zh": "已编辑文件 '%s'：%s（变更 %s）"},
	"no_matches_for_arg_in_rev_arg":                  {"en": "no matches for '%s' in rev '%s'.", "zh": "在版本 '%[2]s' 中未找到 '%[1]s' 的匹配。"},
	"no_organizations_or_repositories":               {"en": "no organizations or repositories.", "zh": "没有组织或仓库。"},
	"no_commits_in_graph":                            {"en": "no commits in graph.", "zh": "图中无提交。"},
	"no_diff_between_arg_and_arg_arg":                {"en": "no diff between '%s' and '%s' (%s).", "zh": "'%s' 与 '%s' 之间无差异（%s）。"},
	"diff_arg_between_arg_arg_n_arg":                 {"en": "diff (%s) between '%s'..'%s':\\n%s", "zh": "差异（%s）介于 '%s'..'%s'：\\n%s"},
	"rebased_arg_onto_arg_with_arg_conflict_s_arg":   {"en": "rebased '%s' onto '%s' with %d conflict(s): %s", "zh": "已将 '%s' 变基到 '%s' 上，共 %d 个冲突：%s"},
	"rebased_arg_onto_arg_tip_arg_change_arg":        {"en": "rebased '%s' onto '%s' (tip %s, change %s).", "zh": "已将 '%s' 变基到 '%s'（尖端 %s，变更 %s）。"},
	"resolved_arg_tip_arg_change_arg":                {"en": "resolved '%s' (tip %s, change %s).", "zh": "已解析 '%s'（尖端 %s，变更 %s）。"},
	"no_commits":                                     {"en": "no commits.", "zh": "无提交。"},
	"change_arg_has_no_content_diff":                 {"en": "change '%s' has no content diff.", "zh": "变更 '%s' 没有内容差异。"},
	"changes_of_arg_n_arg":                           {"en": "changes of '%s':\\n%s", "zh": "'%s' 的变更：\\n%s"},
	"missing_path_argument":                          {"en": "missing 'path' argument", "zh": "缺少 'path' 参数"},
	"read_arg_arg":                                   {"en": "read '%s': %v", "zh": "读取 '%s'：%v"},
	"failed_to_write_file_arg":                       {"en": "failed to write file: %v", "zh": "写入文件失败：%v"},
	"failed_to_delete_file_arg":                      {"en": "failed to delete file: %v", "zh": "删除文件失败：%v"},
	"read_arg_before_edit_arg":                       {"en": "read '%s' before edit: %v", "zh": "编辑前读取 '%s'：%v"},
	"failed_to_write_edited_result_arg":              {"en": "failed to write edited result: %v", "zh": "写入编辑结果失败：%v"},
	"failed_to_list_directory_arg":                   {"en": "failed to list directory: %v", "zh": "列出目录失败：%v"},
	"missing_pattern_argument":                       {"en": "missing 'pattern' argument", "zh": "缺少 'pattern' 参数"},
	"search_failed_arg":                              {"en": "search failed: %v", "zh": "搜索失败：%v"},
	"failed_to_browse_structure_arg":                 {"en": "failed to browse structure: %v", "zh": "浏览结构失败：%v"},
	"failed_to_get_graph_arg":                        {"en": "failed to get graph: %v", "zh": "获取图失败：%v"},
	"rev_a_and_rev_b_are_required":                   {"en": "rev_a and rev_b are required", "zh": "rev_a 与 rev_b 均为必填"},
	"failed_to_get_diff_arg":                         {"en": "failed to get diff: %v", "zh": "获取差异失败：%v"},
	"missing_source_argument":                        {"en": "missing 'source' argument", "zh": "缺少 'source' 参数"},
	"rebase_failed_arg":                              {"en": "rebase failed: %v", "zh": "变基失败：%v"},
	"resolve_failed_arg":                             {"en": "resolve failed: %v", "zh": "解析失败：%v"},
	"failed_to_get_blame_arg":                        {"en": "failed to get blame: %v", "zh": "获取 blame 失败：%v"},
	"failed_to_get_commit_history_arg":               {"en": "failed to get commit history: %v", "zh": "获取提交历史失败：%v"},
	"missing_rev_argument":                           {"en": "missing 'rev' argument", "zh": "缺少 'rev' 参数"},
	"failed_to_view_change_arg":                      {"en": "failed to view change: %v", "zh": "查看变更失败：%v"},
	"set_tag_arg":                                    {"en": "set tag '%s'.", "zh": "已创建标签 '%s'。"},
	"opened_change_request_arg_arg_arg":              {"en": "opened change request #%s (%s → %s).", "zh": "已创建合并请求 #%s（%s → %s）。"},
	"commented_on_arg":                               {"en": "commented on #%s.", "zh": "已在 #%s 评论。"},
	"started_branch_arg_with_subsession_arg_it_is_w": {"en": "Started branch '%s' with subsession '%s'. It is working in the background and will open a change request into '%s' when done. END YOUR TURN NOW and wait for the change-request notification — do not poll.", "zh": "已创建分支 '%s' 及其子会话 '%s'。它在后台工作，完成后会向 '%s' 开一个合并请求。请立即结束本轮并等待合并请求通知——不要轮询。"},
	"merged_arg_revision_arg_arg_conflict_s":         {"en": "merged #%s (revision %s, %d conflict(s)).", "zh": "已合并 #%s（revision %s，%d 个冲突）。"},
	"missing_name":                                   {"en": "missing 'name'", "zh": "缺少 'name'"},
	"title_and_target_are_required":                  {"en": "title and target are required", "zh": "title 与 target 为必填"},
	"iid_and_body_are_required":                      {"en": "iid and body are required", "zh": "iid 与 body 为必填"},
	"missing_branch_the_new_branch_name_chosen_by_y": {"en": "missing 'branch' (the new branch name, chosen by you)", "zh": "缺少 'branch'（由你决定的新分支名）"},
	"missing_prompt_the_self_contained_task":         {"en": "missing 'prompt' (the self-contained task)", "zh": "缺少 'prompt'（自包含任务）"},
	"invalid_branch_name_arg_letters_digits_only_no": {"en": "invalid branch name %q (letters/digits/._/- only, no ':' or '..')", "zh": "非法分支名 %q（仅字母/数字/._/-，不含 ':' 或 '..'）"},
	"subsessions_may_only_be_started_from_the_arg_d": {"en": "subsessions may only be started from the '%s' (default-branch) session (this session is on '%s')", "zh": "仅允许从 '%s'（默认分支）会话创建子会话（当前会话在 '%s'）"},
	"branch_arg_already_exists":                      {"en": "branch '%s' already exists", "zh": "分支 '%s' 已存在"},
	"a_session_for_branch_arg_already_exists":        {"en": "a session for branch '%s' already exists", "zh": "分支 '%s' 已有会话"},
	"merge_is_only_allowed_from_the_arg_default_bra": {"en": "merge is only allowed from the '%s' (default-branch) session (this session is on '%s')", "zh": "仅允许 '%s'（默认分支）会话执行合并（当前会话在 '%s'）"},
	"missing_iid":                                    {"en": "missing 'iid'", "zh": "缺少 'iid'"},
}

var tr = i18n.New(catalog, "en")

// localeOf resolves the session's effective locale for a tool call. It reads
// the agent-projected `vars.agent.locale` (provider "agent") and falls back to
// the env default.
func localeOf(ctx context.Context, ext *extension.Extension, tenant, sessionName, fallback string) string {
	if ext == nil || sessionName == "" {
		return fallback
	}
	v := ext.GetSessionVariable(ctx, tenant, "agent", sessionName, "locale", "")
	if v == "" {
		return fallback
	}
	return v
}

// loc is the session's locale (env-fallback), the single decision input.
func loc(ctx context.Context, ext *extension.Extension, tenant, sessionName string) string {
	return localeOf(ctx, ext, tenant, sessionName, envOr("LOCALE", "en"))
}

// lc returns the localized Content for a tool result (no format args).
func lc(ctx context.Context, ext *extension.Extension, tenant, sessionName string, key MsgKey) string {
	return tr.T(loc(ctx, ext, tenant, sessionName), key, nil)
}

// lcf is lc with printf-style args.
func lcf(ctx context.Context, ext *extension.Extension, tenant, sessionName string, key MsgKey, args ...interface{}) string {
	f := tr.T(loc(ctx, ext, tenant, sessionName), key, nil)
	if len(args) == 0 {
		return f
	}
	return fmt.Sprintf(f, args...)
}

// ef builds a localized error (printf-style args).
func ef(ctx context.Context, ext *extension.Extension, tenant, sessionName string, key MsgKey, args ...interface{}) error {
	return fmt.Errorf("%s", lcf(ctx, ext, tenant, sessionName, key, args...))
}
