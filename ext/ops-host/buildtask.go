package main

import (
	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
	"github.com/abcp-sdk/abc-protocol-go/extension"

	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// buildMaxLogLines caps how many log lines a build task retains in memory.
// Tasks are kept after completion so a page refresh can recover the full log
// (up to this cap); the oldest lines are dropped beyond the cap.
const buildMaxLogLines = 100_000

// buildMaxTasks caps the number of retained tasks (running + finished). The
// oldest finished tasks are evicted beyond the cap, bounding memory.
const buildMaxTasks = 50

// buildTaskTTL bounds how long a finished task is retained in memory before it
// is eligible for eviction (in addition to the task-count cap).
const buildTaskTTL = time.Hour

type buildLogLine struct {
	Stream string `json:"stream"`
	Line   string `json:"line"`
}

type buildTask struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"` // "build" | "publish"
	Tag   string `json:"tag"`
	State string `json:"state"` // "running" | "done" | "failed"

	Image      string     `json:"image,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`

	mu   sync.Mutex
	logs []buildLogLine
	subs map[chan buildLogLine]struct{}
}

// append records a log line, evicts the oldest beyond the cap, and fans the
// line out to active SSE subscribers.
func (t *buildTask) append(line buildLogLine) {
	t.mu.Lock()
	t.logs = append(t.logs, line)
	if len(t.logs) > buildMaxLogLines {
		t.logs = t.logs[len(t.logs)-buildMaxLogLines:]
	}
	subs := make([]chan buildLogLine, 0, len(t.subs))
	for c := range t.subs {
		subs = append(subs, c)
	}
	t.mu.Unlock()
	for _, c := range subs {
		select {
		case c <- line:
		default:
		}
	}
}

func (t *buildTask) subscribe() (chan buildLogLine, func()) {
	c := make(chan buildLogLine, 256)
	t.mu.Lock()
	t.subs[c] = struct{}{}
	t.mu.Unlock()
	return c, func() {
		t.mu.Lock()
		delete(t.subs, c)
		t.mu.Unlock()
	}
}

func (t *buildTask) snapshotLogs() []buildLogLine {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]buildLogLine(nil), t.logs...)
}

func (t *buildTask) setState(state string) {
	t.mu.Lock()
	t.State = state
	now := time.Now()
	if t.FinishedAt == nil {
		t.FinishedAt = &now
	}
	t.mu.Unlock()
}

func (t *buildTask) setResult(image, errStr string) {
	t.mu.Lock()
	t.Image = image
	t.Error = errStr
	if errStr == "" {
		t.State = "done"
	} else {
		t.State = "failed"
	}
	now := time.Now()
	t.FinishedAt = &now
	t.mu.Unlock()
}

// summary returns the task's public JSON view.
func (t *buildTask) summary() map[string]interface{} {
	return map[string]interface{}{
		"id":          t.ID,
		"kind":        t.Kind,
		"tag":         t.Tag,
		"state":       t.State,
		"image":       t.Image,
		"error":       t.Error,
		"started_at":  t.StartedAt,
		"finished_at": t.FinishedAt,
		"log_lines":   len(t.snapshotLogs()),
	}
}

func (s *server) evictBuilds() {
	// Bound the number of retained tasks: evict oldest finished tasks beyond
	// buildMaxTasks, and any finished task older than buildTaskTTL.
	type entry struct {
		id   string
		task *buildTask
	}
	var all []entry
	s.builds.Range(func(k, v interface{}) bool {
		all = append(all, entry{id: k.(string), task: v.(*buildTask)})
		return true
	})
	now := time.Now()
	kept := 0
	for _, e := range all {
		t := e.task
		if t.State == "running" {
			kept++
			continue
		}
		if t.FinishedAt != nil && now.Sub(*t.FinishedAt) > buildTaskTTL {
			s.builds.Delete(e.id)
			continue
		}
		kept++
	}
	if kept <= buildMaxTasks {
		return
	}
	// Evict oldest finished first.
	var finished []entry
	for _, e := range all {
		if e.task.State != "running" {
			finished = append(finished, e)
		}
	}
	sort.Slice(finished, func(i, j int) bool {
		ti, tj := finished[i].task, finished[j].task
		if ti.FinishedAt == nil || tj.FinishedAt == nil {
			return false
		}
		return ti.FinishedAt.Before(*tj.FinishedAt)
	})
	excess := kept - buildMaxTasks
	for i := 0; i < excess && i < len(finished); i++ {
		s.builds.Delete(finished[i].id)
	}
}

// buildsList returns all retained build tasks.
func (s *server) buildsList(w http.ResponseWriter, r *http.Request) {
	var out []map[string]interface{}
	s.builds.Range(func(k, v interface{}) bool {
		out = append(out, v.(*buildTask).summary())
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		si, _ := out[i]["started_at"].(time.Time)
		sj, _ := out[j]["started_at"].(time.Time)
		return si.After(sj)
	})
	writeJSON(w, http.StatusOK, map[string]interface{}{"builds": out})
}

// buildGet returns one task's status plus its full log.
func (s *server) buildGet(w http.ResponseWriter, r *http.Request) {
	v, ok := s.builds.Load(param(r, "id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "build not found")
		return
	}
	t := v.(*buildTask)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"build": t.summary(),
		"logs":  t.snapshotLogs(),
	})
}

// buildStream is an SSE endpoint streaming a build's log: it replays existing
// lines then pushes new ones as they arrive.
func (s *server) buildStream(w http.ResponseWriter, r *http.Request) {
	v, ok := s.builds.Load(param(r, "id"))
	if !ok {
		writeErr(w, http.StatusNotFound, "build not found")
		return
	}
	t := v.(*buildTask)

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	writeEvent := func(event string, data interface{}) {
		b, _ := json.Marshal(data)
		_, _ = w.Write([]byte("event: " + event + "\ndata: " + string(b) + "\n\n"))
		flusher.Flush()
	}

	// Replay history.
	for _, ln := range t.snapshotLogs() {
		writeEvent("log", ln)
	}
	writeEvent("state", map[string]interface{}{"state": t.State})

	// Subscribe for incremental lines.
	ch, unsub := t.subscribe()
	defer unsub()

	// If the task is already finished, send a final state event and return.
	if t.State != "running" {
		writeEvent("done", map[string]interface{}{"state": t.State, "image": t.Image, "error": t.Error})
		return
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ln := <-ch:
			writeEvent("log", ln)
			if t.State != "running" {
				writeEvent("done", map[string]interface{}{"state": t.State, "image": t.Image, "error": t.Error})
				return
			}
		}
	}
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Split(s, "\n")
}

// awaitBuild polls a submitted build task until it finishes, then returns a
// human-readable result. Used by the NATS container-build tool so the agent
// still gets a synchronous outcome even though the HTTP endpoint is async.
func (s *server) awaitBuild(ctx context.Context, id, callID string) (extension.ToolResultData, error) {
	start := time.Now()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		v, ok := s.builds.Load(id)
		if !ok {
			return extension.ToolResultData{}, fmt.Errorf("build %s not found", id)
		}
		t := v.(*buildTask)
		if s.ext != nil && t.State == "running" {
			// progress telemetry: consumed by the orchestration/UI layer,
			// never the LLM context
			_ = s.ext.ReportProgress(ctx, callID, abcprotocol.ToolProgress{
				Phase: protocolPtr("running"),
				Text:  protocolPtr(fmt.Sprintf("%s %s running (%.0fs)", t.Kind, t.Tag, time.Since(start).Seconds())),
			})
		}
		if t.State != "running" {
			if t.Error != "" {
				return extension.ToolResultData{}, fmt.Errorf("%s failed: %s", t.Kind, t.Error)
			}
			return extension.ToolResultData{Content: lc(ctx, s.ext, "", fmt.Sprintf("Finished %s %q", t.Kind, t.Tag), fmt.Sprintf("已完成 %s %q", t.Kind, t.Tag))}, nil
		}
		select {
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				return extension.ToolResultData{}, fmt.Errorf("interrupted: %w", ctx.Err())
			}
			return extension.ToolResultData{}, ctx.Err()
		case <-tick.C:
		}
	}
}

// publishTaskBody carries a package-publish request into the async task.
type publishTaskBody struct {
	Protocol   string
	Org        string
	Repo       string
	Branch     string
	Session    string
	Name       string
	Version    string
	File       string
	Dockerfile string
}

// startPublishTask registers a publish task and runs it in a background
// goroutine. It shares the same buildTask machinery (SSE stream + status).
func (s *server) startPublishTask(b publishTaskBody) string {
	id := uuid.NewString()
	tag := b.Protocol
	if b.Name != "" && b.Version != "" {
		tag = b.Protocol + " " + b.Name + "@" + b.Version
	}
	t := &buildTask{
		ID:        id,
		Kind:      "publish",
		Tag:       tag,
		State:     "running",
		StartedAt: time.Now(),
		subs:      map[chan buildLogLine]struct{}{},
	}
	s.builds.Store(id, t)
	s.evictBuilds()

	go s.runPublishTask(t, b)
	return id
}

func (s *server) runPublishTask(t *buildTask, b publishTaskBody) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	org, repo, branch := b.Org, b.Repo, b.Branch
	if org == "" || repo == "" {
		if b.Session != "" {
			sorg, srepo, sbm, ok := parseSessionName(b.Session)
			if !ok {
				err := fmt.Errorf("session %q is not named org:repo:branch", b.Session)
				t.append(buildLogLine{Stream: "publish", Line: "ERROR: " + err.Error()})
				t.setResult("", err.Error())
				return
			}
			ws, err := s.lookupWorkspace(ctx, b.Session, sorg, srepo, sbm)
			if err != nil {
				t.append(buildLogLine{Stream: "publish", Line: "ERROR: " + err.Error()})
				t.setResult("", err.Error())
				return
			}
			if org == "" {
				org = ws.org
			}
			if repo == "" {
				repo = ws.repo
			}
			if branch == "" {
				branch = ws.branch
			}
		}
	}

	res, err := s.publishPackage(ctx, b.Protocol, org, repo, branch, b.Name, b.Version, b.File, b.Dockerfile)
	if err != nil {
		t.append(buildLogLine{Stream: "publish", Line: "ERROR: " + err.Error()})
		t.setResult("", err.Error())
		return
	}
	t.append(buildLogLine{Stream: "publish", Line: res})
	t.setResult(b.Protocol, "")
}

func protocolPtr(s string) *string { return &s }
