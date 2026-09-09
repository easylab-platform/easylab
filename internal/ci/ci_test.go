package ci

import (
    "context"
    "testing"
)

func TestScore(t *testing.T) {
    rn := &Runner{ID:"r1", Labels: []string{"os=linux","arch=amd64","is_container=true"}, Toolchains: map[string]string{"go":"golang:1.26"}}
    if s := Score(rn, []string{"os=linux","is_container=true"}); s < 0 { t.Fatalf("should match, score=%d", s) }
    if s := Score(rn, []string{"os=windows"}); s >= 0 { t.Fatalf("should NOT match windows, score=%d", s) }
}

func TestToolchainMapping(t *testing.T) {
    if ToolchainImage("go") == "" { t.Fatal("go mapping empty") }
    // no container + toolchain -> default image
    j := &Job{RunsOn: []string{"os=linux","toolchain=go"}}
    if img := ImageFor(j, &Runner{}); img != Toolchains["go"] { t.Fatalf("img=%q", img) }
    // explicit container overrides
    j2 := &Job{RunsOn: []string{"os=linux","toolchain=go"}, Container: "my:latest"}
    if img := ImageFor(j2, &Runner{}); img != "my:latest" { t.Fatalf("override img=%q", img) }
}

func TestMatchExcludesSessionBound(t *testing.T) {
    reg := NewRunnerRegistry()
    reg.Register(&Runner{ID:"ci1", Labels: []string{"os=linux"}})
    reg.Register(&Runner{ID:"sbx", Labels: []string{"os=linux"}, SessionBound:true})
    m := reg.Match([]string{"os=linux"})
    if m == nil || m.ID != "ci1" { t.Fatalf("matched %+v (should be ci1 only)", m) }
}

func TestSchedulerNeeds(t *testing.T) {
    reg := NewRunnerRegistry()
    reg.Register(&Runner{ID:"h", Labels: []string{"os=linux"}, Toolchains: map[string]string{}})
    bk := &fakeBackend{}
    sched := NewScheduler(reg, bk, nil)
    wf := &Workflow{ID:"w", Jobs: []Job{
        {ID:"a", RunsOn: []string{"os=linux"}, Steps: []Step{{Run:"echo a"}}},
        {ID:"b", Needs: []string{"a"}, RunsOn: []string{"os=linux"}, Steps: []Step{{Run:"echo b"}}},
    }}
    run, err := sched.Schedule(contextWithTest(), wf)
    if err != nil { t.Fatalf("sched: %v", err) }
    if run.State != StateSuccess { t.Fatalf("state=%s", run.State) }
    if len(run.Jobs) != 2 { t.Fatalf("jobs=%d", len(run.Jobs)) }
    if run.Jobs[0].State != StateSuccess || run.Jobs[1].State != StateSuccess { t.Fatal("both must succeed") }
}

func TestSchedulerFailureStops(t *testing.T) {
    reg := NewRunnerRegistry(); reg.Register(&Runner{ID:"h", Labels: []string{"os=linux"}})
    bk := &fakeBackend{failOn:"b"}
    sched := NewScheduler(reg, bk, nil)
    wf := &Workflow{ID:"w", Jobs: []Job{
        {ID:"a", Needs: []string{}, RunsOn: []string{"os=linux"}, Steps: []Step{{Run:"echo a"}}},
        {ID:"b", Needs: []string{"a"}, RunsOn: []string{"os=linux"}, Steps: []Step{{Run:"echo b"}}},
    }}
    run, err := sched.Schedule(contextWithTest(), wf)
    if err == nil { t.Fatal("expected failure") }
    if run.State != StateFailure { t.Fatalf("state=%s", run.State) }
    if run.Jobs[1].State != StateFailure { t.Fatal("b must be failure") }
}

type fakeBackend struct{ failOn string }
func (f *fakeBackend) Run(ctx context.Context, job *Job, rn *Runner, onLog func(string)) (string, error) {
    if f.failOn == job.ID { return "boom", nil }
    return "ok", nil
}

func contextWithTest() context.Context { return context.Background() }
