package ci

import "sync"

// JobLogHub is the live log bus for CI jobs: a bounded tail per job plus a
// fan-out to stream subscribers, mirroring ops.Task. A subscriber is
// backfilled with the tail first, then receives live lines until the job's
// terminal state event. It is process-local and ephemeral.
type JobLogHub struct {
	mu   sync.Mutex
	jobs map[string]*jobLog
}

// JobLogEvent is one streamed event: a log line, or a terminal state change.
type JobLogEvent struct {
	Line   string // log text (empty for a pure state event)
	State  string // "" for log lines; "success"|"failure"|"cancelled" on terminal
	Closed bool   // true on the final event (after state is set)
}

type jobLog struct {
	mu     sync.Mutex
	lines  []string
	subs   map[chan JobLogEvent]struct{}
	state  string
	closed bool
}

const jobLogTail = 500

func NewJobLogHub() *JobLogHub { return &JobLogHub{jobs: map[string]*jobLog{}} }

func (h *JobLogHub) get(jobID string) *jobLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	j := h.jobs[jobID]
	if j == nil {
		j = &jobLog{subs: map[chan JobLogEvent]struct{}{}}
		h.jobs[jobID] = j
	}
	return j
}

// Log appends a line and fans it out. Safe before any subscriber exists.
func (h *JobLogHub) Log(jobID, line string) {
	j := h.get(jobID)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.lines = append(j.lines, line)
	if len(j.lines) > jobLogTail {
		j.lines = j.lines[len(j.lines)-jobLogTail:]
	}
	ev := JobLogEvent{Line: line}
	for ch := range j.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Finish sets the terminal state and closes every subscriber after delivering
// the final state event.
func (h *JobLogHub) Finish(jobID, state string) {
	j := h.get(jobID)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.state = state
	j.closed = true
	for ch := range j.subs {
		select {
		case ch <- JobLogEvent{State: state, Closed: true}:
		default:
		}
		close(ch)
		delete(j.subs, ch)
	}
}

// Subscribe returns a channel backfilled with the current tail + state, then
// live. The cancel func unsubscribes (idempotent).
func (h *JobLogHub) Subscribe(jobID string) (<-chan JobLogEvent, func()) {
	j := h.get(jobID)
	ch := make(chan JobLogEvent, 256)
	j.mu.Lock()
	for _, l := range j.lines {
		select {
		case ch <- JobLogEvent{Line: l}:
		default:
		}
	}
	if j.state != "" {
		ch <- JobLogEvent{State: j.state, Closed: true}
		close(ch)
	} else {
		j.subs[ch] = struct{}{}
	}
	j.mu.Unlock()
	cancel := func() {
		j.mu.Lock()
		defer j.mu.Unlock()
		if _, ok := j.subs[ch]; ok {
			delete(j.subs, ch)
			close(ch)
		}
	}
	return ch, cancel
}
