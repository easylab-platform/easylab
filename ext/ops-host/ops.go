package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
)

// Thin easylab /ops client: ops-extension owns the tool/agent semantics and the
// Containerfile/publish templates; easylab owns the k8s/buildkit/helm execution.
// Every call goes to the easylab base (token already injected by addAuth) at
// /api/v1/ops/*, so this struct is a purpose-neutral wrapper over the generic
// primitives the server exposes.

// opsSubmitBuild enqueues an image build on easylab and returns the task id.
func (s *server) opsSubmitBuild(ctx context.Context, req map[string]interface{}) (string, error) {
	if req["namespace"] == nil {
		req["namespace"] = s.runtimeNamespace
	}
	body, err := s.httpPostJSON(ctx, s.base+"/api/v1/ops/builds", req)
	if err != nil {
		return "", err
	}
	var out struct {
		BuildID string `json:"build_id"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || out.BuildID == "" {
		return "", fmt.Errorf("no build_id in %s", body)
	}
	return out.BuildID, nil
}

// opsTask polls a easylab task until it leaves "running", returning its summary.
func (s *server) opsTask(ctx context.Context, id string) (map[string]interface{}, error) {
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		body, err := s.httpGetJSON(ctx, s.base+"/api/v1/ops/tasks/"+url.PathEscape(id))
		if err != nil {
			return nil, err
		}
		var t struct {
			Status string `json:"status"`
			Result string `json:"result"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal([]byte(body), &t); err != nil {
			return nil, fmt.Errorf("ops task %s: bad body: %w", id, err)
		}
		if t.Status != "running" {
			return map[string]interface{}{
				"status": t.Status, "result": t.Result, "error": t.Error,
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

// awaitOpsTask drains an ops task to completion and renders the agent result.
// `callID` (when non-empty) enables progress telemetry on abc.tool.progress.<callID>
// and maps an interrupt-driven cancellation into an "interrupted" error so the
// agent observes the abort rather than a bare context deadline.
func (s *server) awaitOpsTask(ctx context.Context, kind, tag, id, callID string) (string, error) {
	start := time.Now()
	// The tool already has a bounded caller timeout in most cases; add a hard
	// ceiling so a stuck build can never hang a tool call forever.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	res, err := s.opsTask(ctx, id)
	if err != nil {
		if ctx.Err() == context.Canceled {
			return "", fmt.Errorf("interrupted: %w", ctx.Err())
		}
		return "", err
	}
	if res["status"] != "done" {
		if msg, _ := res["error"].(string); msg != "" {
			return "", fmt.Errorf("%s failed: %s", kind, msg)
		}
		return "", fmt.Errorf("%s failed", kind)
	}
	_ = start
	return fmt.Sprintf("Finished %s %q", kind, tag), nil
}

// awaitOpsTaskProgress is awaitOpsTask with progress telemetry (and the same
// interrupt→"interrupted" mapping) for long-running tools that publish
// abc.tool.progress.<callID>. callID must be non-empty to report.
func (s *server) awaitOpsTaskProgress(ctx context.Context, kind, tag, id, callID string) (string, error) {
	start := time.Now()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	for {
		res, err := s.opsTaskOnce(ctx, id)
		if err != nil {
			if ctx.Err() == context.Canceled {
				return "", fmt.Errorf("interrupted: %w", ctx.Err())
			}
			return "", err
		}
		if res["status"] != "done" {
			if s.ext != nil && callID != "" && res["status"] == "running" {
				_ = s.ext.ReportProgress(ctx, callID, abcprotocol.ToolProgress{
					Phase: protocolPtr("running"),
					Text:  protocolPtr(fmt.Sprintf("%s %s running (%.0fs)", kind, tag, time.Since(start).Seconds())),
				})
			}
			select {
			case <-ctx.Done():
				if ctx.Err() == context.Canceled {
					return "", fmt.Errorf("interrupted: %w", ctx.Err())
				}
				return "", ctx.Err()
			case <-tick.C:
			}
			continue
		}
		if msg, _ := res["error"].(string); msg != "" {
			return "", fmt.Errorf("%s failed: %s", kind, msg)
		}
		return fmt.Sprintf("Finished %s %q", kind, tag), nil
	}
}

// opsTaskOnce polls a easylab ops task once (no loop); opsTask is the loop form.
func (s *server) opsTaskOnce(ctx context.Context, id string) (map[string]interface{}, error) {
	body, err := s.httpGetJSON(ctx, s.base+"/api/v1/ops/tasks/"+url.PathEscape(id))
	if err != nil {
		return nil, err
	}
	var t struct {
		Status string `json:"status"`
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &t); err != nil {
		return nil, fmt.Errorf("ops task %s: bad body: %w", id, err)
	}
	return map[string]interface{}{
		"status": t.Status, "result": t.Result, "error": t.Error,
	}, nil
}
