package opsext

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
	MsgContainerSearchFailedArg                                                                                                 MsgKey = "container_search_failed_arg"
	MsgPackageSearchFailedArg                                                                                                   MsgKey = "package_search_failed_arg"
	MsgPullGitRepoMissingGitUrl                                                                                                 MsgKey = "pull_git_repo_missing_git_url"
	MsgCannotInferRepoNameFromArg                                                                                               MsgKey = "cannot_infer_repo_name_from_arg"
	MsgCreateRepoFailedArg                                                                                                      MsgKey = "create_repo_failed_arg"
	MsgSetMirrorFailedArg                                                                                                       MsgKey = "set_mirror_failed_arg"
	MsgCiRunPresetContainerBuildRequiresTag                                                                                     MsgKey = "ci_run_preset_container_build_requires_tag"
	MsgCiRunUnknownPresetArgContainerBuildProt                                                                                  MsgKey = "ci_run_unknown_preset_arg_container_build_prot"
	MsgCiRunWorkflowFailedArg                                                                                                   MsgKey = "ci_run_workflow_failed_arg"
	MsgCiRunTriggerFailedArg                                                                                                    MsgKey = "ci_run_trigger_failed_arg"
	MsgCiRunFailedArg                                                                                                           MsgKey = "ci_run_failed_arg"
	MsgCiRunArg                                                                                                                 MsgKey = "ci_run_arg"
	MsgDeployedArgFromArgInClusterAddressHttp                                                                                   MsgKey = "deployed_arg_from_arg_in_cluster_address_http_"
	MsgServiceArgBelongsToADifferentSessionArg                                                                                  MsgKey = "service_arg_belongs_to_a_different_session_arg"
	MsgServiceDeployFailedArg                                                                                                   MsgKey = "service_deploy_failed_arg"
	MsgServiceListFailedArg                                                                                                     MsgKey = "service_list_failed_arg"
	MsgSandboxEditReadFailedArg                                                                                                 MsgKey = "sandbox_edit_read_failed_arg"
	MsgSandboxEditWriteFailedArg                                                                                                MsgKey = "sandbox_edit_write_failed_arg"
	MsgPortSandboxStatFailedArg                                                                                                 MsgKey = "port_sandbox_stat_failed_arg"
	MsgPortSandboxReadFailedArg                                                                                                 MsgKey = "port_sandbox_read_failed_arg"
	MsgPortWriteFailedArg                                                                                                       MsgKey = "port_write_failed_arg"
	MsgPortSandboxListFailedArg                                                                                                 MsgKey = "port_sandbox_list_failed_arg"
	MsgPortSandboxDirectoryArgIsEmpty                                                                                           MsgKey = "port_sandbox_directory_arg_is_empty"
	MsgPortCommitWriteFailedArg                                                                                                 MsgKey = "port_commit_write_failed_arg"
	MsgCreatedArgSandboxFromArgContainerArgSta                                                                                  MsgKey = "created_arg_sandbox_from_arg_container_arg_sta"
	MsgDownloadedFileArgSandboxPathArgArgBytes                                                                                  MsgKey = "downloaded_file_arg_sandbox_path_arg_arg_bytes"
	MsgWroteSandboxFileArg                                                                                                      MsgKey = "wrote_sandbox_file_arg"
	MsgSandboxCreateMissingImageBaseImageEasyla                                                                                 MsgKey = "sandbox_create_missing_image_base_image_easyla"
	MsgSandboxCreateFailedArg                                                                                                   MsgKey = "sandbox_create_failed_arg"
	MsgSandboxRunMissingCommand                                                                                                 MsgKey = "sandbox_run_missing_command"
	MsgSandboxRunSyncFailedArg                                                                                                  MsgKey = "sandbox_run_sync_failed_arg"
	MsgSandboxRunFailedArg                                                                                                      MsgKey = "sandbox_run_failed_arg"
	MsgSandboxReadFailedArg                                                                                                     MsgKey = "sandbox_read_failed_arg"
	MsgSandboxDownloadCodeAndPathAreRequired                                                                                    MsgKey = "sandbox_download_code_and_path_are_required"
	MsgSandboxDownloadDownloadArgArg                                                                                            MsgKey = "sandbox_download_download_arg_arg"
	MsgSandboxDownloadFailedArg                                                                                                 MsgKey = "sandbox_download_failed_arg"
	MsgSandboxWriteFailedArg                                                                                                    MsgKey = "sandbox_write_failed_arg"
	MsgSandboxJobListFailedArg                                                                                                  MsgKey = "sandbox_job_list_failed_arg"
	MsgSandboxJobOutputFailedArg                                                                                                MsgKey = "sandbox_job_output_failed_arg"
	MsgSandboxJobWaitFailedArg                                                                                                  MsgKey = "sandbox_job_wait_failed_arg"
	MsgSandboxJobStdinFailedArg                                                                                                 MsgKey = "sandbox_job_stdin_failed_arg"
	MsgSandboxJobKillFailedArg                                                                                                  MsgKey = "sandbox_job_kill_failed_arg"
	MsgMirroringArgArgFromArgPullScheduled                                                                                      MsgKey = "mirroring_arg_arg_from_arg_pull_scheduled"
	MsgCiLaunchedPresetArgRunArgArg                                                                                             MsgKey = "ci_launched_preset_arg_run_arg_arg"
	MsgLaunchedArgRunSFromEasylabWorkflowsYamlArg                                                                               MsgKey = "launched_arg_run_s_from_easylab_workflows_yaml_arg"
	MsgSkippedArg                                                                                                               MsgKey = "skipped_arg"
	MsgEditedSandboxFileArg                                                                                                     MsgKey = "edited_sandbox_file_arg"
	MsgPortedArgToRepoArgChangeArg                                                                                              MsgKey = "ported_arg_to_repo_arg_change_arg"
	MsgPortedDirectoryArgToRepoArgArgFileSChangeArg                                                                             MsgKey = "ported_directory_arg_to_repo_arg_arg_file_s_change_arg"
	MsgBackgroundCommandFinishedJobArgExitArg                                                                                   MsgKey = "background_command_finished_job_arg_exit_arg"
	MsgBackgroundCommandWaitFailedJobArgArgTheJobItselfMayStillBeRunningInspectItWithSandboxJobOutputOrStopItWithSandboxJobKill MsgKey = "background_command_wait_failed_job_arg_arg_the_job_itself_may_still_be_running_inspect_it_with_sandbox_job_output_or_stop_it_with_sandbox_job_kill"
	MsgCommandCompletedJobArgExitArg                                                                                            MsgKey = "command_completed_job_arg_exit_arg"
	MsgJobArgStillRunningInBackgroundWaitWindowArgmsElapsedDriveItViaJobId                                                      MsgKey = "job_arg_still_running_in_background_wait_window_argms_elapsed_drive_it_via_job_id"
)

// catalog holds this extension's en/zh templates. Extend with more locales by
// adding a column to each entry.
var catalog = i18n.Catalog{
	"container_search_failed_arg":                            {"en": "container-search failed: %v", "zh": "container-search 失败：%v"},
	"package_search_failed_arg":                              {"en": "package-search failed: %v", "zh": "package-search 失败：%v"},
	"pull_git_repo_missing_git_url":                          {"en": "pull-git-repo: missing 'git_url'", "zh": "pull-git-repo：缺少 'git_url'"},
	"cannot_infer_repo_name_from_arg":                        {"en": "cannot infer repo name from %s", "zh": "无法从 %s 推导仓库名"},
	"create_repo_failed_arg":                                 {"en": "create repo failed: %v", "zh": "创建仓库失败：%v"},
	"set_mirror_failed_arg":                                  {"en": "set mirror failed: %v", "zh": "设置镜像失败：%v"},
	"ci_run_preset_container_build_requires_tag":             {"en": "ci-run: preset container-build requires 'tag'", "zh": "ci-run：预设 container-build 需要 'tag'"},
	"ci_run_unknown_preset_arg_container_build_prot":         {"en": "ci-run: unknown preset %q (container-build | <protocol>-publish)", "zh": "ci-run：未知预设 %q（container-build | <protocol>-publish）"},
	"ci_run_workflow_failed_arg":                             {"en": "ci-run workflow failed: %v", "zh": "ci-run 工作流失败：%v"},
	"ci_run_trigger_failed_arg":                              {"en": "ci-run trigger failed: %v", "zh": "ci-run 触发失败：%v"},
	"ci_run_failed_arg":                                      {"en": "ci-run failed: %v", "zh": "ci-run 失败：%v"},
	"ci_run_arg":                                             {"en": "ci-run: %s", "zh": "ci-run：%s"},
	"deployed_arg_from_arg_in_cluster_address_http_":         {"en": "Deployed '%s' from %s. In-cluster address: http://%s:80 (ready=%s). The sandbox can reach it via this hostname.", "zh": "已从 %[2]s 部署 '%[1]s'。集群内地址：http://%[3]s:80（ready=%[4]s）。沙箱可直接用该主机名访问。"},
	"service_arg_belongs_to_a_different_session_arg":         {"en": "service '%s' belongs to a different session (%s); use another name", "zh": "服务 '%s' 属于其它会话（%s）；请换一个名字"},
	"service_deploy_failed_arg":                              {"en": "service-deploy failed: %v", "zh": "service-deploy 失败：%v"},
	"service_list_failed_arg":                                {"en": "service-list failed: %v", "zh": "service-list 失败：%v"},
	"sandbox_edit_read_failed_arg":                           {"en": "sandbox edit read failed: %v", "zh": "sandbox 编辑读取失败：%v"},
	"sandbox_edit_write_failed_arg":                          {"en": "sandbox edit write failed: %v", "zh": "sandbox 编辑写入失败：%v"},
	"port_sandbox_stat_failed_arg":                           {"en": "port sandbox stat failed: %v", "zh": "沙箱 stat 失败：%v"},
	"port_sandbox_read_failed_arg":                           {"en": "port sandbox read failed: %v", "zh": "沙箱读取失败：%v"},
	"port_write_failed_arg":                                  {"en": "port write failed: %v", "zh": "沙箱写入失败：%v"},
	"port_sandbox_list_failed_arg":                           {"en": "port sandbox list failed: %v", "zh": "沙箱列表失败：%v"},
	"port_sandbox_directory_arg_is_empty":                    {"en": "port sandbox directory '%s' is empty", "zh": "沙箱目录 '%s' 为空"},
	"port_commit_write_failed_arg":                           {"en": "port commit write failed: %v", "zh": "提交写入失败：%v"},
	"created_arg_sandbox_from_arg_container_arg_sta":         {"en": "Created %s sandbox from %s (container %s, status %s).", "zh": "已从 %[2]s 创建 %[1]s 沙箱（容器 %[3]s，状态 %[4]s）。"},
	"downloaded_file_arg_sandbox_path_arg_arg_bytes":         {"en": "Downloaded file %s → sandbox path '%s' (%d bytes).", "zh": "已将文件 %s 下载到沙箱路径 '%s'（%d 字节）。"},
	"wrote_sandbox_file_arg":                                 {"en": "Wrote sandbox file '%s'.", "zh": "已写入沙箱文件 '%s'。"},
	"sandbox_create_missing_image_base_image_easyla":         {"en": "sandbox-create: missing 'image' (base image easylab can pull)", "zh": "sandbox-create：缺少 'image'（easylab 可拉取的基础镜像）"},
	"sandbox_create_failed_arg":                              {"en": "sandbox-create failed: %v", "zh": "sandbox-create 失败：%v"},
	"sandbox_run_missing_command":                            {"en": "sandbox-run: missing 'command'", "zh": "sandbox-run：缺少 'command'"},
	"sandbox_run_sync_failed_arg":                            {"en": "sandbox-run sync failed: %v", "zh": "sandbox-run 同步失败：%v"},
	"sandbox_run_failed_arg":                                 {"en": "sandbox-run failed: %v", "zh": "sandbox-run 失败：%v"},
	"sandbox_read_failed_arg":                                {"en": "sandbox-read failed: %v", "zh": "sandbox-read 失败：%v"},
	"sandbox_download_code_and_path_are_required":            {"en": "sandbox-download: 'code' and 'path' are required", "zh": "sandbox-download：'code' 与 'path' 均为必填"},
	"sandbox_download_download_arg_arg":                      {"en": "sandbox-download: download %s: %v", "zh": "sandbox-download：下载 %s：%v"},
	"sandbox_download_failed_arg":                            {"en": "sandbox-download failed: %v", "zh": "sandbox-download 失败：%v"},
	"sandbox_write_failed_arg":                               {"en": "sandbox-write failed: %v", "zh": "sandbox-write 失败：%v"},
	"sandbox_job_list_failed_arg":                            {"en": "sandbox-job-list failed: %v", "zh": "sandbox-job-list 失败：%v"},
	"sandbox_job_output_failed_arg":                          {"en": "sandbox-job-output failed: %v", "zh": "sandbox-job-output 失败：%v"},
	"sandbox_job_wait_failed_arg":                            {"en": "sandbox-job-wait failed: %v", "zh": "sandbox-job-wait 失败：%v"},
	"sandbox_job_stdin_failed_arg":                           {"en": "sandbox-job-stdin failed: %v", "zh": "sandbox-job-stdin 失败：%v"},
	"sandbox_job_kill_failed_arg":                            {"en": "sandbox-job-kill failed: %v", "zh": "sandbox-job-kill 失败：%v"},
	"mirroring_arg_arg_from_arg_pull_scheduled":              {"en": "mirroring %s/%s from %s (pull scheduled)", "zh": "正在从 %s 镜像 %s/%s（已安排拉取）"},
	"ci_launched_preset_arg_run_arg_arg":                     {"en": "CI launched (preset %s, run %s%s).", "zh": "CI 已启动（预设 %s，运行 %s%s）。"},
	"launched_arg_run_s_from_easylab_workflows_yaml_arg":     {"en": "Launched %d run(s) from .easylab/workflows.yaml: %s", "zh": "已从 .easylab/workflows.yaml 启动 %d 个运行：%s"},
	"skipped_arg":                                            {"en": " (skipped: %s)", "zh": "（已跳过：%s）"},
	"edited_sandbox_file_arg":                                {"en": "Edited sandbox file '%s'.", "zh": "已编辑沙箱文件 '%s'。"},
	"ported_arg_to_repo_arg_change_arg":                      {"en": "Ported '%s' to repo '%s' (change %s).", "zh": "已将 '%s' 移植到仓库 '%s'（变更 %s）。"},
	"ported_directory_arg_to_repo_arg_arg_file_s_change_arg": {"en": "Ported directory '%s' to repo '%s' (%d file(s), change %s).", "zh": "已将目录 '%s' 移植到仓库 '%s'（%d 个文件，变更 %s）。"},
	"background_command_finished_job_arg_exit_arg":           {"en": "Background command finished (job %s, exit %d)", "zh": "后台命令已结束（任务 %s，退出码 %d）"},
	"background_command_wait_failed_job_arg_arg_the_job_itself_may_still_be_running_inspect_it_with_sandbox_job_output_or_stop_it_with_sandbox_job_kill": {"en": "Background command wait failed (job %s): %v. The job itself may still be running; inspect it with sandbox-job-output or stop it with sandbox-job-kill.", "zh": "后台命令等待失败（任务 %s）：%v。任务本身可能仍在运行；用 sandbox-job-output 查看或用 sandbox-job-kill 停止。"},
	"command_completed_job_arg_exit_arg":                                                {"en": "Command completed (job %s, exit %d)", "zh": "命令已完成（任务 %s，退出码 %d）"},
	"job_arg_still_running_in_background_wait_window_argms_elapsed_drive_it_via_job_id": {"en": "Job %s still running in background (wait window %dms elapsed); drive it via job-id", "zh": "任务 %s 仍在后台运行（等待窗口已过 %dms）；请通过 job-id 继续操作"},
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

// lcf is lc with printf-style args. The template is a catalog constant, so the
// %-verbs are applied via fmt.Sprintf here rather than at the call site.
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
