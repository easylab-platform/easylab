// Package ops implements the EasyVCS dev/deploy platform runtime substrate.
//
// It is the shared seam that lets easylab schedule work either as local
// subprocesses (default, self-contained) or on a Kubernetes cluster (easylab
// parity, optional). Following easylab's rule, the server process never
// executes user code: a Run is dispatched to a Runtime backend (a local worker
// subprocess or a cluster sandbox), and its progress is streamed through an
// in-memory Task registry.
package ops

import (
	"sync"
	"time"
)

// TaskKind classifies a long-running operation for filtering.
type TaskKind string

// Task kinds.
const (
	KindRun   TaskKind = "run"   // one-shot command
	KindBuild TaskKind = "build" // container image build
	KindSvc   TaskKind = "service"
)

// TaskState is the lifecycle state of a task.
type TaskState string

// Task states.
const (
	StateRunning   TaskState = "running"
	StateSucceeded TaskState = "succeeded"
	StateFailed    TaskState = "failed"
)

// TaskEvent is one line of live progress (SSE payload).
type TaskEvent struct {
	Event string `json:"event"` // "log" | "state"
	Data  string `json:"data"`
	State string `json:"state,omitempty"`
}

// Task is a single tracked operation with a bounded log tail and an SSE
// broadcast bus. It is process-local and ephemeral by design (a restart drops
// it); durable state lives in the SQLite run/job tables where it matters.
type Task struct {
	ID      string
	Kind    TaskKind
	mu      sync.RWMutex
	state   TaskState
	logs    []string
	tx      chan TaskEvent
	subs    map[chan TaskEvent]struct{}
	created time.Time
}

// appendLog bounds the in-memory log and fan-outs to subscribers.
func (t *Task) appendLog(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.logs = append(t.logs, line)
	if len(t.logs) > 500 {
		t.logs = t.logs[len(t.logs)-500:]
	}
	ev := TaskEvent{Event: "log", Data: line}
	for ch := range t.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// setState updates the state and broadcasts it.
func (t *Task) setState(s TaskState) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state = s
	ev := TaskEvent{Event: "state", Data: string(s), State: string(s)}
	for ch := range t.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Log records a progress line.
func (t *Task) Log(line string) { t.appendLog(line) }

// Finish records success/failure and a final state.
func (t *Task) Finish(ok bool, result, errMsg string) {
	if ok {
		t.setState(StateSucceeded)
	} else {
		t.setState(StateFailed)
	}
	if result != "" {
		t.appendLog(result)
	}
	if errMsg != "" {
		t.appendLog(errMsg)
	}
}

// Subscribe returns a receive-only channel of events, backfilled with the
// current log tail, then live.
func (t *Task) Subscribe() (<-chan TaskEvent, func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ch := make(chan TaskEvent, 64)
	t.subs[ch] = struct{}{}
	// Backfill the bounded tail.
	for _, l := range t.logs {
		ch <- TaskEvent{Event: "log", Data: l}
	}
	ch <- TaskEvent{Event: "state", Data: string(t.state), State: string(t.state)}
	cancel := func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		delete(t.subs, ch)
	}
	return ch, cancel
}

// Snapshot is a point-in-time view of a task.
func (t *Task) Snapshot() TaskEvent {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return TaskEvent{Event: "state", Data: string(t.state), State: string(t.state)}
}

// State returns the current state.
func (t *Task) State() TaskState {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state
}

// TaskRegistry tracks in-flight/completed tasks.
type TaskRegistry struct {
	mu    sync.Mutex
	tasks map[string]*Task
	seq   int64
}

// NewTaskRegistry builds an empty registry.
func NewTaskRegistry() *TaskRegistry {
	return &TaskRegistry{tasks: map[string]*Task{}}
}

// NewID generates a short unique task id.
func (r *TaskRegistry) NewID(prefix string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return prefix + "-" + itoa(r.seq)
}

// Create registers a new task and returns it.
func (r *TaskRegistry) Create(id string, kind TaskKind) *Task {
	t := &Task{
		ID:      id,
		Kind:    kind,
		state:   StateRunning,
		tx:      make(chan TaskEvent, 256),
		subs:    map[chan TaskEvent]struct{}{},
		created: time.Now().UTC(),
	}
	r.mu.Lock()
	r.tasks[id] = t
	r.mu.Unlock()
	return t
}

// Get returns a task by id.
func (r *TaskRegistry) Get(id string) *Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.tasks[id]
}

// List returns all tasks, newest first.
func (r *TaskRegistry) List() []*Task {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*Task, 0, len(r.tasks))
	for _, t := range r.tasks {
		out = append(out, t)
	}
	return out
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
