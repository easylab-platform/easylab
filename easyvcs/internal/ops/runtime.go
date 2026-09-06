package ops

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// RunSpec describes a one-shot command to execute.
type RunSpec struct {
	// Command is the shell command (run via `sh -c`).
	Command string
	// WorkDir is the working directory, if any.
	WorkDir string
	// Env is a set of extra environment variables (KEY=VAL).
	Env []string
	// Timeout, if >0, bounds the run.
	Timeout time.Duration
}

// RunResult is the outcome of a run.
type RunResult struct {
	ExitCode int
	Output   string
}

// Runtime is the execution seam. A LocalRuntime runs commands as subprocesses
// in this process's working environment; a cluster backend (future) could
// dispatch to a Kubernetes sandbox. The server never runs user code directly —
// it always goes through a Runtime.
type Runtime interface {
	// Run executes a one-shot command, streaming progress lines to `log`. It
	// returns the exit code and buffered output on completion.
	Run(ctx context.Context, spec RunSpec, log func(string)) (RunResult, error)
	// Name identifies the backend.
	Name() string
}

// --- local backend (self-contained, default) ---

// LocalRuntime executes commands as subprocesses in a restricted working
// directory. It is the default backend so the platform is fully self-contained
// without a cluster.
type LocalRuntime struct {
	workRoot string
}

// NewLocalRuntime creates a local runtime rooted at workRoot for scratch work.
func NewLocalRuntime(workRoot string) *LocalRuntime {
	return &LocalRuntime{workRoot: workRoot}
}

// Name implements Runtime.
func (r *LocalRuntime) Name() string { return "local" }

// Run implements Runtime: it launches `sh -c <command>` in a temp workdir,
// streams stdout+stderr to log, and returns the exit code.
func (r *LocalRuntime) Run(ctx context.Context, spec RunSpec, log func(string)) (RunResult, error) {
	if spec.Command == "" {
		return RunResult{}, fmt.Errorf("empty command")
	}
	dir := spec.WorkDir
	if dir == "" {
		dir = r.workRoot
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return RunResult{}, err
	}
	ctx2, cancel := context.WithCancel(ctx)
	defer cancel()
	if spec.Timeout > 0 {
		ctx2, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx2, "sh", "-c", spec.Command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), spec.Env...)
	// Put the child in its own process group so a timeout kills the whole tree
	// (sh and any grandchildren like `sleep`), not just the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Kill the process group (negative pid = group leader).
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 500 * time.Millisecond

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return RunResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return RunResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return RunResult{}, err
	}

	var output strings.Builder
	scan := func(rd io.Reader) {
		sc := bufio.NewScanner(rd)
		for sc.Scan() {
			line := sc.Text()
			output.WriteString(line + "\n")
			if log != nil {
				log(line)
			}
		}
	}
	done := make(chan struct{}, 2)
	go func() { scan(stdout); done <- struct{}{} }()
	go func() { scan(stderr); done <- struct{}{} }()
	<-done
	<-done

	err = cmd.Wait()
	if ctx2.Err() != nil && errors.Is(ctx2.Err(), context.DeadlineExceeded) {
		return RunResult{ExitCode: -1, Output: output.String()}, fmt.Errorf("run timed out")
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return RunResult{ExitCode: ee.ExitCode(), Output: output.String()}, nil
		}
		return RunResult{ExitCode: -1, Output: output.String()}, err
	}
	return RunResult{ExitCode: 0, Output: output.String()}, nil
}

// LocalRuntimeAlias keeps the name explicit for callers that only want local.
var _ Runtime = (*LocalRuntime)(nil)

// scratchDir creates a unique directory under the work root.
func scratchDir(root, tag string) (string, error) {
	d := filepath.Join(root, tag+"-"+fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}
