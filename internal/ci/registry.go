package ci

import (
	"fmt"
	"sync"
	"time"
)

// RunnerRegistry holds the CI pool: self-registered, label-bearing, heartbeat
// runners with session_bound=false. Sandbox workers (session_bound=true) are
// tracked by the WorkerRegistry (dev world) and never join this pool.
type RunnerRegistry struct {
	mu      sync.RWMutex
	runners map[string]*Runner
}

func NewRunnerRegistry() *RunnerRegistry {
	return &RunnerRegistry{runners: map[string]*Runner{}}
}

// Register upserts a runner (host runners call on boot + heartbeat).
func (r *RunnerRegistry) Register(rn *Runner) error {
	if rn.ID == "" {
		return fmt.Errorf("runner id required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rn.LastSeen.IsZero() {
		rn.LastSeen = time.Now()
	}
	existing := r.runners[rn.ID]
	if existing != nil {
		rn.Labels = existing.Labels // keep registered capability labels stable
	}
	r.runners[rn.ID] = rn
	return nil
}

// Heartbeat refreshes a runner's last-seen.
func (r *RunnerRegistry) Heartbeat(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	rn, ok := r.runners[id]
	if ok {
		rn.LastSeen = time.Now()
	}
	return ok
}

// Unregister removes a runner (explicit shutdown).
func (r *RunnerRegistry) Unregister(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.runners, id)
}

// list returns a snapshot (thread-safe).
func (r *RunnerRegistry) list() []*Runner {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Runner, 0, len(r.runners))
	for _, rn := range r.runners {
		c := *rn
		out = append(out, &c)
	}
	return out
}

// Score computes how well a runner's labels satisfy a job's required labels.
// A score < 0 means incompatible; higher = better fit (fewer unmet).
func Score(rn *Runner, required []string) int {
	score := 0
	for _, req := range required {
		// required form: key=value ; runner matching supports prefix.
		kv := splitKV(req)
		if kv == nil {
			score++ // plain label must be present
			if !hasLabel(rn.Labels, req) {
				return -1
			}
			continue
		}
		key, val := kv[0], kv[1]
		rv := runnerLabel(rn.Labels, key)
		if key == "toolchain" {
			// toolchain satisfied if runner has it OR its Toolchains map knows
			// a default image (scheduler will inject a container).
			if rv == val {
				continue
			}
			if _, ok := rn.Toolchains[val]; ok {
				continue
			}
			if _, ok := Toolchains[val]; ok {
				continue
			}
			// xcode: preinstalled on mac host (val empty image) — treat as
			// satisfiable if runner os=macos and toolchain unknown-but-hint.
			if val == "xcode" && runnerLabel(rn.Labels, "os") == "macos" {
				continue
			}
			return -1
		}
		if rv != val {
			return -1
		}
		score++
	}
	return score
}

// Match returns the best runner for a job's required labels (or nil).
func (r *RunnerRegistry) Match(labels []string) *Runner {
	r.mu.RLock()
	defer r.mu.RUnlock()
	best := (*Runner)(nil)
	bestScore := -1
	for _, rn := range r.runners {
		if rn.SessionBound {
			continue // CI pool excludes sandbox workers
		}
		if time.Since(rn.LastSeen) > 2*time.Minute {
			continue // stale
		}
		s := Score(rn, labels)
		if s >= 0 && s > bestScore {
			best = rn
			bestScore = s
		}
	}
	return best
}

// List returns non-session-bound runners for ListRunners.
func (r *RunnerRegistry) List() []*Runner {
	out := r.list()
	keep := out[:0]
	for _, rn := range out {
		if !rn.SessionBound {
			keep = append(keep, rn)
		}
	}
	sortRunner(keep)
	return keep
}

func splitKV(s string) []string {
	for i := 0; i < len(s); i++ {
		if s[i] == '=' {
			return []string{s[:i], s[i+1:]}
		}
	}
	return nil
}

func hasLabel(labels []string, l string) bool {
	for _, x := range labels {
		if x == l {
			return true
		}
	}
	return false
}

func runnerLabel(labels []string, key string) string {
	for _, l := range labels {
		if i := stringsIndexByte(l, '='); i > 0 && l[:i] == key {
			return l[i+1:]
		}
	}
	return ""
}

func stringsIndexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

func sortRunner(r []*Runner) {
	for i := 1; i < len(r); i++ {
		for j := i; j > 0 && r[j].ID < r[j-1].ID; j-- {
			r[j], r[j-1] = r[j-1], r[j]
		}
	}
}
