package ops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newFakePodman writes a shell script that records its argv to a log file and
// emulates the podman subcommands Launch relies on (network inspect/create/rm,
// run -d, inspect). Callers assert on the recorded args.
func newFakePodman(t *testing.T) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "podman.log")
	bin = filepath.Join(dir, "podman")
	script := `#!/bin/sh
echo "$@" >> "` + logPath + `"
case "$1" in
  network)
    case "$2" in
      inspect) echo "someid" ;;        # pretend network exists
      create)  echo "$4" ;;            # print network name
      rm)      echo "$4" ;;
      *)       echo "ok" ;;
    esac ;;
  run)
    echo "fake-container-id" ;;
  inspect)
    echo "10.0.0.2" ;;
  *)
    echo "" ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EASYVCS_PODMAN_BIN", bin)
	return bin, logPath
}

func logOf(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func hasArg(t *testing.T, log, arg string) bool {
	t.Helper()
	return strings.Contains(log, arg)
}

// TestPodmanLaunchSharesGroupNetwork verifies that when Group is set, the two
// services are launched on the same network (`--network <group>`), enabling
// same-network DNS name resolution.
func TestPodmanLaunchSharesGroupNetwork(t *testing.T) {
	_, logPath := newFakePodman(t)
	r := NewPodmanServiceRunner("127.0.0.1:8080", t.TempDir())

	if _, err := r.Launch(t.Context(), ServiceRequest{
		Name: "svcA", Group: "app", Image: "busybox",
		Command: "sleep 3600", CPUs: "0.5", MemoryBytes: 134217728,
	}, nil); err != nil {
		t.Fatalf("launch svcA: %v", err)
	}
	if _, err := r.Launch(t.Context(), ServiceRequest{
		Name: "svcB", Group: "app", Image: "busybox",
	}, nil); err != nil {
		t.Fatalf("launch svcB: %v", err)
	}

	log := logOf(t, logPath)
	if !hasArg(t, log, "network create --driver bridge app") {
		t.Fatalf("expected a shared 'app' network create, got:\n%s", log)
	}
	if !hasArg(t, log, "--network app") {
		t.Fatalf("expected a --network app on run, got:\n%s", log)
	}
	// Resource limits should reach the container for svcA.
	if !hasArg(t, log, "--cpus 0.5") || !hasArg(t, log, "--memory 134217728") {
		t.Fatalf("expected resource limits on launch, got:\n%s", log)
	}
}

// TestPodmanFallbackPerServiceNetwork verifies a service without Group gets its
// own per-name network.
func TestPodmanFallbackPerServiceNetwork(t *testing.T) {
	_, logPath := newFakePodman(t)
	r := NewPodmanServiceRunner("127.0.0.1:8080", t.TempDir())
	if _, err := r.Launch(t.Context(), ServiceRequest{Name: "solo", Image: "busybox"}, nil); err != nil {
		t.Fatalf("launch: %v", err)
	}
	log := logOf(t, logPath)
	if !hasArg(t, log, "--network solo") {
		t.Fatalf("expected per-service network 'solo', got:\n%s", log)
	}
}
