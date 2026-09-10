package ci

import (
	"testing"
	"time"
)

func TestJobLogHubBackfillAndLive(t *testing.T) {
	h := NewJobLogHub()
	h.Log("j1", "early-1")
	h.Log("j1", "early-2")

	ch, cancel := h.Subscribe("j1")
	defer cancel()

	// backfilled tail
	for _, want := range []string{"early-1", "early-2"} {
		select {
		case ev := <-ch:
			if ev.Line != want {
				t.Fatalf("backfill = %q want %q", ev.Line, want)
			}
		case <-time.After(time.Second):
			t.Fatal("backfill timeout")
		}
	}
	// live
	h.Log("j1", "live")
	select {
	case ev := <-ch:
		if ev.Line != "live" {
			t.Fatalf("live = %q", ev.Line)
		}
	case <-time.After(time.Second):
		t.Fatal("live timeout")
	}
	// terminal
	h.Finish("j1", "success")
	select {
	case ev := <-ch:
		if !ev.Closed || ev.State != "success" {
			t.Fatalf("terminal = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("terminal timeout")
	}
}

func TestJobLogHubSubscribeAfterFinish(t *testing.T) {
	h := NewJobLogHub()
	h.Log("j2", "line")
	h.Finish("j2", "failure")
	ch, cancel := h.Subscribe("j2")
	defer cancel()
	var gotLine, gotState bool
	for ev := range ch {
		if ev.Line == "line" {
			gotLine = true
		}
		if ev.Closed && ev.State == "failure" {
			gotState = true
		}
	}
	if !gotLine || !gotState {
		t.Fatalf("replay missing: line=%v state=%v", gotLine, gotState)
	}
}
