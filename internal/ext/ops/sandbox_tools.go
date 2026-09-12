package opsext

import (
	"context"
	"errors"

	"connectrpc.com/connect"
	"fmt"
	"strings"
	"time"

	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
	"github.com/abcp-sdk/abc-protocol-go/extension"
	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	workerv1 "github.com/easylab-platform/easylab-proto/worker/v1"
)

// runViaGateway executes a command in the sandbox through the easylab
// SandboxService gateway (single entry). Every command becomes a job; we
// sync-wait for the terminal output tail.
func (s *server) runViaGateway(ctx context.Context, cid, command string, timeoutMs int) (jobID string, done JobDone, err error) {
	execRes, err := s.sdk.Sandbox.Execute(ctx, connect.NewRequest(&easylabv1.ExecuteRequest{
		Sandbox: cid, Req: &workerv1.ExecuteRequest{Command: command},
	}))
	if err != nil {
		return "", done, err
	}
	jobID = execRes.Msg.GetJobId()

	// Sync-wait: the worker always registers a backgrounded job; JobWait
	// blocks until completion or timeout (worker caps 60s). Then fold the
	// output tails into the tool result. A wait deadline is NOT a failure:
	// the job stays registered and running in the background — surface it as
	// such so long jobs (sleep/serve) keep their handle for stdin/kill.
	waitCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	wait, err := s.sdk.Sandbox.JobWait(waitCtx, connect.NewRequest(&easylabv1.JobWaitRequest{
		Sandbox: cid, Req: &workerv1.JobWaitRequest{JobId: jobID, TimeoutMs: int32(timeoutMs)},
	}))
	if err != nil {
		if !errors.Is(err, waitCtx.Err()) && !errors.Is(err, context.DeadlineExceeded) && connect.CodeOf(err) != connect.CodeDeadlineExceeded && connect.CodeOf(err) != connect.CodeCanceled {
			return "", done, err
		}
		done.Bg = true
	}
	if wait != nil && wait.Msg != nil {
		done.ExitCode = wait.Msg.ExitCode
	}
	// Pull the full output tail (stdout+stderr joined) for the tool result.
	out, oerr := s.sdk.Sandbox.JobOutput(ctx, connect.NewRequest(&easylabv1.JobOutputRequest{
		Sandbox: cid, Req: &workerv1.JobOutputRequest{JobId: jobID, Start: -200, End: 0},
	}))
	if oerr == nil && out != nil && out.Msg != nil {
		done.Stdout = strings.Join(out.Msg.Lines, "\n")
	}
	return jobID, done, nil
}

// backgroundWatch polls a job to completion and notifies via the session
// mailbox — replaces the legacy SSE background watcher.
func (s *server) backgroundWatch(ctx context.Context, cid, jobID, sid string) {
	bgCtx, bgCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer bgCancel()
	// Poll JobWait in 5s slices; when done, fetch output and mail it.
	lastErr := error(nil)
	for {
		w, err := s.sdk.Sandbox.JobWait(bgCtx, connect.NewRequest(&easylabv1.JobWaitRequest{
			Sandbox: cid, Req: &workerv1.JobWaitRequest{JobId: jobID, TimeoutMs: 5000},
		}))
		if err != nil {
			lastErr = err
			break
		}
		if w.Msg != nil && w.Msg.State != "running" {
			resp, oerr := s.sdk.Sandbox.JobOutput(bgCtx, connect.NewRequest(&easylabv1.JobOutputRequest{
				Sandbox: cid, Req: &workerv1.JobOutputRequest{JobId: jobID, Start: -200, End: 0},
			}))
			lines := []string(nil)
			if oerr == nil && resp != nil && resp.Msg != nil {
				lines = resp.Msg.Lines
			}
			msg := fmt.Sprintf("Background command finished (job %s, exit %d)", jobID, w.Msg.ExitCode)
			if s := strings.Join(lines, "\n"); s != "" {
				msg += "\n" + s
			}
			if s.ext != nil {
				_ = s.ext.PublishMailboxEvent(context.Background(), sid, "event",
					map[string]interface{}{"content": msg})
			}
			return
		}
		select {
		case <-bgCtx.Done():
			lastErr = bgCtx.Err()
			// fallthrough to error notify
		default:
		}
	}
	// Never report a fabricated "finished (exit 0)" when the wait broke.
	if s.ext != nil && lastErr != nil {
		_ = s.ext.PublishMailboxEvent(context.Background(), sid, "event",
			map[string]interface{}{"content": fmt.Sprintf(
				"Background command wait failed (job %s): %v. The job itself may still be running; inspect it with sandbox-job-output or stop it with sandbox-job-kill.",
				jobID, lastErr)})
	}
	_ = ctx
}

func (s *server) registerSandboxTools(m map[string]extension.ToolSpec) {

	m["sandbox-create"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			image := strArg(args, "image")
			runtime := strArg(args, "runtime")
			if runtime == "" {
				runtime = "linux"
			}
			// VM runtimes (windows/macos) carry their own image; only the linux
			// (derived) path needs a base image.
			if image == "" && runtime == "linux" {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-create: missing 'image' (base image easylab can pull)", "sandbox-create：缺少 'image'（easylab 可拉取的基础镜像）")
			}
			ws, sid, err := s.resolveWorkspace(ctx, args, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			info, err := s.launchWorkspaceSandbox(ctx, ws, sid, image, runtime)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-create failed: %v", "sandbox-create 失败：%v", err)
			}
			s.publishSandboxVars(ctx, sid, info)
			label := image
			if label == "" {
				label = runtime
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName,
				fmt.Sprintf("Created %s sandbox from %s (container %s, status %s).", runtime, label, info.ContainerID, info.Status),
				fmt.Sprintf("已从 %s 创建 %s 沙箱（容器 %s，状态 %s）。", label, runtime, info.ContainerID, info.Status))}, nil
		},
	}

	m["sandbox-run"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			command := strArg(args, "command")
			if command == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-run: missing 'command'", "sandbox-run：缺少 'command'")
			}
			sc, err := s.ensureSandbox(ctx, args, sessionName, true)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			// Rev coherence is owned by easylab (SyncWorkspace). Always
			// ensure synced first; failure surfaces a clear error.
			if err := s.ensureSynced(ctx, sc.cid, sc.session, sc.ws); err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-run sync failed: %v", "sandbox-run 同步失败：%v", err)
			}

			timeoutMs := int(abcprotocol.ArgInt(args, "timeout-ms", 10000))
			if timeoutMs <= 0 {
				timeoutMs = 10000
			}
			jobID, done, err := s.runViaGateway(ctx, sc.cid, command, timeoutMs)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-run failed: %v", "sandbox-run 失败：%v", err)
			}
			content := fmt.Sprintf("Command completed (job %s, exit %d)", jobID, done.ExitCode)
			if done.Bg {
				content = fmt.Sprintf("Job %s still running in background (wait window %dms elapsed); drive it via job-id", jobID, timeoutMs)
			}
			if done.Stdout != "" {
				content += "\n" + done.Stdout
			}
			return extension.ToolResultData{Content: content, Data: map[string]interface{}{
				"job-id":       jobID,
				"exit_code":    done.ExitCode,
				"backgrounded": done.Bg,
			}}, nil
		},
	}

	m["sandbox-read"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			path := strArg(args, "path")
			sc, err := s.ensureSandbox(ctx, args, sessionName, true)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			data, err := s.sandboxFileRead(ctx, sc.cid, path)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-read failed: %v", "sandbox-read 失败：%v", err)
			}
			return extension.ToolResultData{Content: string(data)}, nil
		},
	}
	m["sandbox-download"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			code := strArg(args, "code")
			path := strArg(args, "path")
			if code == "" || path == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-download: 'code' and 'path' are required", "sandbox-download：'code' 与 'path' 均为必填")
			}
			sc, err := s.ensureSandbox(ctx, args, sessionName, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			data, err := s.fetchAgentFile(ctx, code)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-download: download %s: %v", "sandbox-download：下载 %s：%v", code, err)
			}
			if err := s.sandboxFileWrite(ctx, sc.cid, path, data); err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-download failed: %v", "sandbox-download 失败：%v", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("Downloaded file %s → sandbox path '%s' (%d bytes).", code, path, len(data)), fmt.Sprintf("已将文件 %s 下载到沙箱路径 '%s'（%d 字节）。", code, path, len(data)))}, nil
		},
	}
	m["sandbox-write"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			path := strArg(args, "path")
			sc, err := s.ensureSandbox(ctx, args, sessionName, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			if err := s.sandboxFileWrite(ctx, sc.cid, path, []byte(strArg(args, "content"))); err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-write failed: %v", "sandbox-write 失败：%v", err)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName, fmt.Sprintf("Wrote sandbox file '%s'.", path), fmt.Sprintf("已写入沙箱文件 '%s'。", path))}, nil
		},
	}
	m["sandbox-edit"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			path := strArg(args, "path")
			startLine := intArg64(args, "start-line", 0)
			endLine := intArg64(args, "end-line", 0)
			content := strArg(args, "content")
			sc, err := s.ensureSandbox(ctx, args, sessionName, true)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			v, err := s.sandboxEdit(ctx, sessionName, sc.cid, path, startLine, endLine, content)
			return extension.ToolResultData{Content: v}, err
		},
	}
	m["sandbox-job-list"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			sc, err := s.ensureSandbox(ctx, args, sessionName, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			jobs, err := s.sdk.Sandbox.ListJobs(ctx, connect.NewRequest(&easylabv1.ListJobsRequest{Sandbox: sc.cid}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-job-list failed: %v", "sandbox-job-list 失败：%v", err)
			}
			return extension.ToolResultData{Content: toJSON(jobs.Msg.Jobs)}, nil
		},
	}
	m["sandbox-job-output"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			sc, err := s.ensureSandbox(ctx, args, sessionName, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			res, err := s.sdk.Sandbox.JobOutput(ctx, connect.NewRequest(&easylabv1.JobOutputRequest{
				Sandbox: sc.cid,
				Req: &workerv1.JobOutputRequest{
					JobId:  strArg(args, "job-id"),
					Start:  int33(args["start"], 0),
					End:    int33(args["end"], 0),
					Stream: strArg(args, "stream"),
				},
			}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-job-output failed: %v", "sandbox-job-output 失败：%v", err)
			}
			return extension.ToolResultData{Content: toJSON(res.Msg)}, nil
		},
	}
	m["sandbox-job-wait"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			sc, err := s.ensureSandbox(ctx, args, sessionName, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			res, err := s.sdk.Sandbox.JobWait(ctx, connect.NewRequest(&easylabv1.JobWaitRequest{
				Sandbox: sc.cid,
				Req: &workerv1.JobWaitRequest{
					JobId:     strArg(args, "job-id"),
					TimeoutMs: int33(args["timeout-ms"], 0),
				},
			}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-job-wait failed: %v", "sandbox-job-wait 失败：%v", err)
			}
			return extension.ToolResultData{Content: toJSON(res.Msg)}, nil
		},
	}
	m["sandbox-job-stdin"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			sc, err := s.ensureSandbox(ctx, args, sessionName, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			_, err = s.sdk.Sandbox.JobStdin(ctx, connect.NewRequest(&easylabv1.JobStdinRequest{
				Sandbox: sc.cid,
				Req: &workerv1.JobStdinRequest{
					JobId: strArg(args, "job-id"),
					Data:  []byte(strArg(args, "data")),
					Close: boolArg(args, "close"),
				},
			}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-job-stdin failed: %v", "sandbox-job-stdin 失败：%v", err)
			}
			return extension.ToolResultData{Content: `{"ok":true}`}, nil
		},
	}
	m["sandbox-job-kill"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			sc, err := s.ensureSandbox(ctx, args, sessionName, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			_, err = s.sdk.Sandbox.JobKill(ctx, connect.NewRequest(&easylabv1.JobKillRequest{
				Sandbox: sc.cid, Req: &workerv1.JobKillRequest{JobId: strArg(args, "job-id")},
			}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-job-kill failed: %v", "sandbox-job-kill 失败：%v", err)
			}
			return extension.ToolResultData{Content: `{"ok":true}`}, nil
		},
	}
	m["sandbox-port"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			sc, err := s.ensureSandbox(ctx, args, sessionName, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			v, err := s.portFile(ctx, sessionName, sc, args)
			if err == nil {
				s.invalidateWorkspace(sc.session)
			}
			return extension.ToolResultData{Content: v}, err
		},
	}
}

func int33(v interface{}, def int32) int32 {
	switch t := v.(type) {
	case int32:
		return t
	case int64:
		return int32(t)
	case float64:
		return int32(t)
	case int:
		return int32(t)
	case nil:
		return def
	}
	return def
}
