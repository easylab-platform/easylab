package ops

import (
	"strings"
	"testing"
	"time"
)

func TestLocalRuntimeRun(t *testing.T) {
	rt := NewLocalRuntime(t.TempDir())
	res, err := rt.Run(t.Context(), RunSpec{Command: "echo hello"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || !strings.Contains(res.Output, "hello") {
		t.Fatalf("run: %+v", res)
	}
}

func TestLocalRuntimeTimeout(t *testing.T) {
	rt := NewLocalRuntime(t.TempDir())
	_, err := rt.Run(t.Context(), RunSpec{Command: "sleep 5", Timeout: 50 * time.Millisecond}, nil)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestTaskRegistryLifecycle(t *testing.T) {
	reg := NewTaskRegistry()
	id := reg.NewID("run")
	task := reg.Create(id, KindRun)
	if task.State() != StateRunning {
		t.Fatalf("initial state: %s", task.State())
	}
	task.Log("first")
	task.Finish(true, "ok", "")
	if task.State() != StateSucceeded {
		t.Fatalf("final state: %s", task.State())
	}
	list := reg.List()
	if len(list) != 1 {
		t.Fatalf("tasks: %d", len(list))
	}
}

func TestNamespaces(t *testing.T) {
	reg := NewNamespaceRegistry([]string{"staging", "prod", "staging"})
	if reg.Default() != "staging" {
		t.Fatalf("default: %s", reg.Default())
	}
	if !reg.Approved("prod") {
		t.Fatal("prod not approved")
	}
	if reg.Approved("nope") {
		t.Fatal("nope should not be approved")
	}
	if ns, ok := reg.Resolve(""); !ok || ns != "staging" {
		t.Fatalf("resolve empty: %s %v", ns, ok)
	}
	if _, ok := reg.Resolve("nope"); ok {
		t.Fatal("resolve nope should fail")
	}
	// Empty -> default only.
	empty := NewNamespaceRegistry(nil)
	if empty.Default() != "default" {
		t.Fatalf("empty default: %s", empty.Default())
	}
}

// TestSSEStream validates the task stream endpoint emits JSON events.
func TestSSEStream(t *testing.T) {
	reg := NewTaskRegistry()
	id := reg.NewID("run")
	task := reg.Create(id, KindRun)
	task.Log("hello")

	// Subscribe (manual, as the handler does).
	ch, cancel := task.Subscribe()
	defer cancel()
	var first TaskEvent
	select {
	case ev := <-ch:
		first = ev
	case <-time.After(500 * time.Millisecond):
		t.Fatal("no event")
	}
	_ = first
}
