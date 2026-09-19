package opsext

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"

	abcprotocol "github.com/abcp-sdk/abc-protocol-go/v2"
	"github.com/abcp-sdk/abc-protocol-go/v2/extension"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	workerv1 "github.com/easylab-platform/easylab-proto/worker/v1"
	"github.com/easylab-platform/easylab/internal/ext"
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
func (s *server) backgroundWatch(ctx context.Context, tenant, cid, jobID, sid string) {
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
			msg := lcf(context.Background(), s.ext, tenant, sid, MsgBackgroundCommandFinishedJobArgExitArg, jobID, w.Msg.ExitCode)
			if s := strings.Join(lines, "\n"); s != "" {
				msg += "\n" + s
			}
			if s.ext != nil {
				_ = s.ext.PublishMailboxEvent(context.Background(), tenant, sid, "event",
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
		_ = s.ext.PublishMailboxEvent(context.Background(), tenant, sid, "event",
			map[string]interface{}{"content": lcf(context.Background(), s.ext, tenant, sid, MsgBackgroundCommandWaitFailedJobArgArgTheJobItselfMayStillBeRunningInspectItWithSandboxJobOutputOrStopItWithSandboxJobKill, jobID, lastErr)})
	}
	_ = ctx
}

func (s *server) registerSandboxTools(m map[string]extension.ToolSpec) {

	m["sandbox-create"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			image := strArg(args, "image")
			runtime := strArg(args, "runtime")
			if runtime == "" {
				runtime = "linux"
			}
			// VM runtimes (windows/macos) carry their own image; only the linux
			// (derived) path needs a base image.
			if image == "" && runtime == "linux" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxCreateMissingImageBaseImageEasyla)
			}
			ws, sid, err := s.resolveWorkspace(ctx, args, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			info, err := s.launchWorkspaceSandbox(ctx, tenant, ws, sid, image, runtime)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxCreateFailedArg, err)
			}
			s.publishSandboxVars(ctx, tenant, sid, info)
			label := image
			if label == "" {
				label = runtime
			}
			return extension.ToolResultData{Content: lcf(ctx, s.ext, tenant, sessionName, MsgCreatedArgSandboxFromArgContainerArgSta, runtime, label, info.ContainerID, info.Status)}, nil
		},
	}

	m["sandbox-run"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			command := strArg(args, "command")
			if command == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxRunMissingCommand)
			}
			sc, err := s.ensureSandbox(ctx, args, sessionName, tenant, true)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			// Rev coherence is owned by easylab (SyncWorkspace). Always
			// ensure synced first; failure surfaces a clear error.
			if err := s.ensureSynced(ctx, sc.cid, sc.session, sc.ws); err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxRunSyncFailedArg, err)
			}

			timeoutMs := int(abcprotocol.ArgInt(args, "timeout-ms", 10000))
			if timeoutMs <= 0 {
				timeoutMs = 10000
			}
			jobID, done, err := s.runViaGateway(ctx, sc.cid, command, timeoutMs)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxRunFailedArg, err)
			}
			content := lcf(ctx, s.ext, tenant, sessionName, MsgCommandCompletedJobArgExitArg, jobID, done.ExitCode)
			if done.Bg {
				content = lcf(ctx, s.ext, tenant, sessionName, MsgJobArgStillRunningInBackgroundWaitWindowArgmsElapsedDriveItViaJobId, jobID, timeoutMs)
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
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			path := strArg(args, "path")
			sc, err := s.ensureSandbox(ctx, args, sessionName, tenant, true)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			data, err := s.sandboxFileRead(ctx, sc.cid, path)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxReadFailedArg, err)
			}
			return extension.ToolResultData{Content: string(data)}, nil
		},
	}
	m["sandbox-download"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			code := strArg(args, "code")
			path := strArg(args, "path")
			if code == "" || path == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxDownloadCodeAndPathAreRequired)
			}
			sc, err := s.ensureSandbox(ctx, args, sessionName, tenant, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			data, err := s.fetchAgentFile(ctx, code)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxDownloadDownloadArgArg, code, err)
			}
			if err := s.sandboxFileWrite(ctx, sc.cid, path, data); err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxDownloadFailedArg, err)
			}
			return extension.ToolResultData{Content: lcf(ctx, s.ext, tenant, sessionName, MsgDownloadedFileArgSandboxPathArgArgBytes, code, path, len(data))}, nil
		},
	}
	m["sandbox-write"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			path := strArg(args, "path")
			sc, err := s.ensureSandbox(ctx, args, sessionName, tenant, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			if err := s.sandboxFileWrite(ctx, sc.cid, path, []byte(strArg(args, "content"))); err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxWriteFailedArg, err)
			}
			return extension.ToolResultData{Content: lcf(ctx, s.ext, tenant, sessionName, MsgWroteSandboxFileArg, path)}, nil
		},
	}
	m["sandbox-edit"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			path := strArg(args, "path")
			startLine := intArg64(args, "start-line", 0)
			endLine := intArg64(args, "end-line", 0)
			content := strArg(args, "content")
			sc, err := s.ensureSandbox(ctx, args, sessionName, tenant, true)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			v, err := s.sandboxEdit(ctx, tenant, sessionName, sc.cid, path, startLine, endLine, content)
			return extension.ToolResultData{Content: v}, err
		},
	}
	m["sandbox-job-list"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			sc, err := s.ensureSandbox(ctx, args, sessionName, tenant, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			jobs, err := s.sdk.Sandbox.ListJobs(ctx, connect.NewRequest(&easylabv1.ListJobsRequest{Sandbox: sc.cid}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxJobListFailedArg, err)
			}
			return extension.ToolResultData{Content: toJSON(jobs.Msg.Jobs)}, nil
		},
	}
	m["sandbox-job-output"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			sc, err := s.ensureSandbox(ctx, args, sessionName, tenant, false)
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
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxJobOutputFailedArg, err)
			}
			return extension.ToolResultData{Content: toJSON(res.Msg)}, nil
		},
	}
	m["sandbox-job-wait"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			sc, err := s.ensureSandbox(ctx, args, sessionName, tenant, false)
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
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxJobWaitFailedArg, err)
			}
			return extension.ToolResultData{Content: toJSON(res.Msg)}, nil
		},
	}
	m["sandbox-job-stdin"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			sc, err := s.ensureSandbox(ctx, args, sessionName, tenant, false)
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
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxJobStdinFailedArg, err)
			}
			return extension.ToolResultData{Content: `{"ok":true}`}, nil
		},
	}
	m["sandbox-job-kill"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			sc, err := s.ensureSandbox(ctx, args, sessionName, tenant, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			_, err = s.sdk.Sandbox.JobKill(ctx, connect.NewRequest(&easylabv1.JobKillRequest{
				Sandbox: sc.cid, Req: &workerv1.JobKillRequest{JobId: strArg(args, "job-id")},
			}))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, tenant, sessionName, MsgSandboxJobKillFailedArg, err)
			}
			return extension.ToolResultData{Content: `{"ok":true}`}, nil
		},
	}
	m["sandbox-port"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string, tenant string) (extension.ToolResultData, error) {
			ctx = ext.WithLabTenant(ctx, tenant)
			sc, err := s.ensureSandbox(ctx, args, sessionName, tenant, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			v, err := s.portFile(ctx, tenant, sessionName, sc, args)
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
