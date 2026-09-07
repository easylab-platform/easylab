package main

import (
	"context"
	"easyvcs-ext-ops/internal/worker"
	"errors"
	"fmt"
	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
	"github.com/abcp-sdk/abc-protocol-go/extension"
	"strings"
	"time"
)

func (s *server) registerSandboxTools(m map[string]extension.ToolSpec) {

	m["sandbox-create"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			image := strArg(args, "image")
			if image == "" {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-create: missing 'image' (base image easylab can pull)", "sandbox-create：缺少 'image'（easylab 可拉取的基础镜像）")
			}
			_, sid, err := s.resolveWorkspace(ctx, args, sessionName)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			info, err := s.createWorker(ctx, sid, image)
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-create failed: %v", "sandbox-create 失败：%v", err)
			}
			s.publishSandboxVars(ctx, sid, info)
			return extension.ToolResultData{Content: lc(ctx, s.ext, sessionName,
				fmt.Sprintf("Created sandbox from %s (container %s, status %s).", image, info.ContainerID, info.Status),
				fmt.Sprintf("已从 %s 创建沙箱（容器 %s，状态 %s）。", image, info.ContainerID, info.Status))}, nil
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
			workerURL, err := s.resolveWorkerURL(ctx, sc.cid)
			if err != nil {
				return extension.ToolResultData{}, err
			}

			run := func(rev string) (worker.ExecuteResult, error) {
				return worker.Execute(ctx, worker.ToWsURL(workerURL), command, rev)
			}

			res, err := run(sc.ws.rev)
			if err != nil {
				// Worker may have restarted (synced_rev lost): re-sync once
				// and retry; if it still refuses, execute without the rev
				// gate (content is verified synced on our side).
				if strings.Contains(err.Error(), "need_sync") {
					s.markUnsynced(sc.cid)
					if err := s.ensureSynced(ctx, sc.cid, sc.session, sc.ws); err != nil {
						return extension.ToolResultData{}, err
					}
					if res, err = run(sc.ws.rev); err != nil {
						if res, err = run(""); err != nil {
							return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-run failed: %v", "sandbox-run 失败：%v", err)
						}
					}
				} else {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-run failed: %v", "sandbox-run 失败：%v", err)
				}
			}

			// Sync-wait the job up to timeout_ms (default 10s). The worker
			// always registers a backgrounded job; the per-job SSE stream
			// replays history then streams live output until job.completed.
			// The bus is model-facing and carries no streamed deltas, so
			// the terminal tool result folds the captured output in; the
			// UI reads live output via the gateway's per-worker SSE proxy.
			timeoutMs := int(abcprotocol.ArgInt(args, "timeout-ms", 10000))
			if timeoutMs <= 0 {
				timeoutMs = 10000
			}
			streamCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
			defer cancel()

			type streamResult struct {
				done worker.JobDone
				err  error
			}
			resultCh := make(chan streamResult, 1)
			go func() {
				done, err := worker.StreamJobOutput(streamCtx, workerURL, res.JobID, nil)
				resultCh <- streamResult{done: done, err: err}
			}()

			select {
			case sr := <-resultCh:
				if sr.err != nil && !errors.Is(sr.err, context.DeadlineExceeded) {
					return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-run stream failed: %v", "sandbox-run 流失败：%v", sr.err)
				}
				content := fmt.Sprintf("Command completed (job %s, exit %d)", res.JobID, sr.done.ExitCode)
				if sr.done.Stdout != "" {
					content += "\n" + sr.done.Stdout
				}
				if sr.done.Stderr != "" {
					content += "\n[stderr]\n" + sr.done.Stderr
				}
				return extension.ToolResultData{Content: content, Data: map[string]interface{}{
					"job-id":       res.JobID,
					"exit_code":    sr.done.ExitCode,
					"backgrounded": false,
				}}, nil

			case <-streamCtx.Done():
				// Timed out: hand the job to a background watcher and return
				// immediately. The watcher keeps the SSE stream open until
				// completion, then notifies the agent via the session
				// mailbox (payload.content is folded into the chat).
				if s.ext != nil {
					go func(jobID, sid string) {
						bgCtx, bgCancel := context.WithTimeout(context.Background(), 30*time.Minute)
						defer bgCancel()
						done, streamErr := worker.StreamJobOutput(bgCtx, workerURL, jobID, nil)
						if streamErr != nil {
							// Never report a fabricated "finished (exit 0)"
							// when the stream broke — tell the agent the
							// outcome is unknown and how to inspect it.
							_ = s.ext.PublishMailboxEvent(context.Background(), sid, "event",
								map[string]interface{}{"content": fmt.Sprintf(
									"Background command stream failed (job %s): %v. The job itself may still be running; inspect it with sandbox-job-output or stop it with sandbox-job-kill.",
									jobID, streamErr)})
							return
						}
						msg := fmt.Sprintf("Background command finished (job %s, exit %d)", jobID, done.ExitCode)
						if done.Stdout != "" {
							msg += "\n" + done.Stdout
						}
						if done.Stderr != "" {
							msg += "\n[stderr]\n" + done.Stderr
						}
						_ = s.ext.PublishMailboxEvent(context.Background(), sid, "event",
							map[string]interface{}{"content": msg})
					}(res.JobID, sessionName)
				}
				content := fmt.Sprintf(
					"Command is still running in the background (job %s); it did not finish within %dms. It keeps running in the background and you will be notified on completion. Meanwhile you can inspect current output with sandbox-job-output, or stop it with sandbox-job-kill.",
					res.JobID, timeoutMs)
				return extension.ToolResultData{Content: content, Data: map[string]interface{}{
					"job-id":       res.JobID,
					"backgrounded": true,
				}}, nil
			}
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
			res, err := s.workerCommand(ctx, sc.cid, "jobs", map[string]interface{}{})
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-job-list failed: %v", "sandbox-job-list 失败：%v", err)
			}
			return extension.ToolResultData{Content: toJSON(res)}, nil
		},
	}
	m["sandbox-job-output"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			sc, err := s.ensureSandbox(ctx, args, sessionName, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			res, err := s.workerCommand(ctx, sc.cid, "job_output", jobArgs(args))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-job-output failed: %v", "sandbox-job-output 失败：%v", err)
			}
			return extension.ToolResultData{Content: toJSON(res)}, nil
		},
	}
	m["sandbox-job-wait"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			sc, err := s.ensureSandbox(ctx, args, sessionName, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			res, err := s.workerCommand(ctx, sc.cid, "job_wait", jobArgs(args))
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-job-wait failed: %v", "sandbox-job-wait 失败：%v", err)
			}
			return extension.ToolResultData{Content: toJSON(res)}, nil
		},
	}
	m["sandbox-job-stdin"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			sc, err := s.ensureSandbox(ctx, args, sessionName, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			res, err := s.workerCommand(ctx, sc.cid, "job_stdin", map[string]interface{}{
				"job_id": strArg(args, "job-id"),
				"data":   strArg(args, "data"),
			})
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-job-stdin failed: %v", "sandbox-job-stdin 失败：%v", err)
			}
			return extension.ToolResultData{Content: toJSON(res)}, nil
		},
	}
	m["sandbox-job-kill"] = extension.ToolSpec{
		Execute: func(ctx context.Context, args map[string]interface{}, callID string, sessionName string) (extension.ToolResultData, error) {
			sc, err := s.ensureSandbox(ctx, args, sessionName, false)
			if err != nil {
				return extension.ToolResultData{}, err
			}
			res, err := s.workerCommand(ctx, sc.cid, "kill", map[string]interface{}{
				"job_id": strArg(args, "job-id"),
			})
			if err != nil {
				return extension.ToolResultData{}, ef(ctx, s.ext, sessionName, "sandbox-job-kill failed: %v", "sandbox-job-kill 失败：%v", err)
			}
			return extension.ToolResultData{Content: toJSON(res)}, nil
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
				// The branch moved: forget the cached head so the next
				// call re-syncs and observes the ported file.
				s.invalidateWorkspace(sc.session)
			}
			return extension.ToolResultData{Content: v}, err
		},
	}
}
