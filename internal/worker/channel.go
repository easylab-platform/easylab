// Package worker implements the WebSocket RPC client for the sandbox worker.
package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// wsReadTimeout must exceed the worker's job_wait cap (60s) by enough margin
// that a job_wait response always arrives before this deadline. See
// worker-go rpc.go jobWaitMaxMs.
const wsReadTimeout = 65 * time.Second

// CommandOnce opens a WS connection, sends one RPC command, and awaits the
// matching response. Mirrors executor's channel::command_once.
func CommandOnce(ctx context.Context, wsURL, method string, params map[string]interface{}) (interface{}, error) {
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, _, err := dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("ws connect %s: %w", wsURL, err)
	}
	defer conn.Close()

	id := nextID()
	req := map[string]interface{}{"id": id, "method": method, "params": params}
	if err := conn.WriteJSON(req); err != nil {
		return nil, fmt.Errorf("ws send: %w", err)
	}

	conn.SetReadDeadline(time.Now().Add(wsReadTimeout))
	for {
		var v map[string]interface{}
		if err := conn.ReadJSON(&v); err != nil {
			return nil, fmt.Errorf("ws recv: %w", err)
		}
		// Server-push events (job.output / job.completed) are broadcast on the
		// same socket without an id; skip them and keep waiting for the RPC
		// reply carrying our id.
		if _, isEvent := v["event"]; isEvent {
			continue
		}
		if vid, ok := v["id"].(float64); ok && uint64(vid) == id {
			if e, ok := v["error"].(string); ok && e != "" {
				return nil, fmt.Errorf("%s", e)
			}
			return v["result"], nil
		}
	}
}

// ExecuteResult is the normalized outcome of a streamed execute. Every
// command is a job (the worker has no fast/slow split), so JobID is always
// set.
type ExecuteResult struct {
	JobID string
}

// Execute runs `execute` on the worker, returning the job id. The caller
// then subscribes to the per-job SSE stream via StreamJobOutput.
func Execute(ctx context.Context, wsURL, command, rev string) (ExecuteResult, error) {
	params := map[string]interface{}{"command": command}
	if rev != "" {
		params["rev"] = rev
	}
	res, err := CommandOnce(ctx, wsURL, "execute", params)
	if err != nil {
		return ExecuteResult{}, err
	}
	m, ok := res.(map[string]interface{})
	if !ok {
		return ExecuteResult{}, fmt.Errorf("execute: unexpected response %T", res)
	}
	jid, _ := m["job_id"].(string)
	if jid == "" {
		return ExecuteResult{}, fmt.Errorf("execute: missing job_id in %v", m)
	}
	return ExecuteResult{JobID: jid}, nil
}

// StreamJobOutput opens the worker's per-job SSE stream (/ws/job?job_id=…)
// and invokes onOutput for every job.output event until job.completed arrives
// or ctx is done. Returns the completion event (exit_code, tails). onOutput
// may be nil — the bus carries no tool deltas, so callers that only need the
// terminal result pass nil and read the completion tails from JobDone.
type JobDone struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

func StreamJobOutput(ctx context.Context, workerURL, jobID string, onOutput func(content string)) (JobDone, error) {
	base := strings.TrimSuffix(workerURL, "/")
	url := base + "/ws/job?job_id=" + jobID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return JobDone{}, err
	}
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return JobDone{}, fmt.Errorf("job stream %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return JobDone{}, fmt.Errorf("job stream status %d", resp.StatusCode)
	}

	done := JobDone{ExitCode: -1}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)

	var eventName string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if eventName == "job.output" {
				var ev struct {
					Content string `json:"content"`
				}
				if json.Unmarshal([]byte(payload), &ev) == nil && onOutput != nil {
					fmt.Printf("[worker] job.output %q\n", ev.Content)
					onOutput(ev.Content)
				}
			} else if eventName == "job.completed" {
				var ev struct {
					ExitCode int    `json:"exit_code"`
					Stdout   string `json:"stdout"`
					Stderr   string `json:"stderr"`
				}
				if json.Unmarshal([]byte(payload), &ev) == nil {
					done.ExitCode = ev.ExitCode
					done.Stdout = ev.Stdout
					done.Stderr = ev.Stderr
				}
				return done, nil
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return done, fmt.Errorf("job stream read: %w", err)
	}
	return done, ctx.Err()
}

var idCounter atomic.Uint64

func nextID() uint64 {
	return idCounter.Add(1)
}

// ToWsURL converts a worker http URL to its websocket form.
func ToWsURL(workerURL string) string {
	switch {
	case hasPrefix(workerURL, "https://"):
		return "wss://" + workerURL[len("https://"):] + "/ws"
	case hasPrefix(workerURL, "http://"):
		return "ws://" + workerURL[len("http://"):] + "/ws"
	default:
		return "ws://" + workerURL + "/ws"
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }
