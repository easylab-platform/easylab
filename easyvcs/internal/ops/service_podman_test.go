package ops

import (
	"strings"
	"testing"
)

// TestPodmanServiceRunnerName verifies the podman runner labels itself podman.
func TestPodmanServiceRunnerName(t *testing.T) {
	r := NewPodmanServiceRunner("127.0.0.1:8080", "/tmp")
	if r.Name() != "podman" {
		t.Fatalf("name: %s", r.Name())
	}
}

// TestPodmanQualify verifies bare names get the internal registry prefix.
func TestPodmanQualify(t *testing.T) {
	r := NewPodmanServiceRunner("127.0.0.1:8080", "/tmp")
	if got := r.qualify("busybox"); got != "127.0.0.1:8080/library/busybox:latest" {
		t.Fatalf("bare: %s", got)
	}
	if got := r.qualify("team/app/api:ok"); got != "127.0.0.1:8080/team/app/api:ok" {
		t.Fatalf("ns: %s", got)
	}
	if got := r.qualify("registry.example.com/x:1"); got != "registry.example.com/x:1" {
		t.Fatalf("fq: %s", got)
	}
}

// TestServiceRequestNetworkName verifies group-based shared network naming.
func TestServiceRequestNetworkName(t *testing.T) {
	// Explicit network wins.
	if got := (ServiceRequest{Name: "a", Group: "app", Network: "netx"}).NetworkName(); got != "netx" {
		t.Fatalf("explicit: %s", got)
	}
	// Group drives shared network.
	if got := (ServiceRequest{Name: "a", Group: "app"}).NetworkName(); got != "app" {
		t.Fatalf("group: %s", got)
	}
	// Fall back to name.
	if got := (ServiceRequest{Name: "a"}).NetworkName(); got != "a" {
		t.Fatalf("name: %s", got)
	}
}

// TestInjectedEnvDefaults verifies a launched service container gets plugin
// registry env + the upstream proxy without any per-container config.
func TestInjectedEnvDefaults(t *testing.T) {
	r := NewPodmanServiceRunnerWithProxy("127.0.0.1:8080", "/tmp",
		"http://easylab.temp.svc.cluster.local:8080",
		"http://mihomo.develop.svc.cluster.local:7890")
	env := r.injectedEnv(nil)
	joined := strings.Join(env, "\n")

	// Pull-through package registries point at easylab.
	for _, want := range []string{
		"NPM_CONFIG_REGISTRY=", "PIP_INDEX_URL=", "GOPROXY=",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %s in env:\n%s", want, joined)
		}
	}
	// General outbound goes through the upstream proxy.
	if !strings.Contains(joined, "HTTPS_PROXY=http://mihomo.develop.svc.cluster.local:7890") {
		t.Fatalf("missing HTTPS_PROXY in env:\n%s", joined)
	}
	// NO_PROXY covers cluster + easylab self-base, but not the proxy.
	if !strings.Contains(joined, "NO_PROXY=127.0.0.1,localhost,.svc.cluster.local,.svc,easylab.temp.svc.cluster.local") {
		t.Fatalf("missing NO_PROXY in env:\n%s", joined)
	}
}

// TestInjectedEnvExplicitWins verifies caller-provided env is never overwritten.
func TestInjectedEnvExplicitWins(t *testing.T) {
	r := NewPodmanServiceRunnerWithProxy("127.0.0.1:8080", "/tmp",
		"http://easylab.temp.svc.cluster.local:8080", "http://proxy:3128")
	env := r.injectedEnv([]string{"HTTPS_PROXY=http://custom:9000", "GOPROXY=off"})
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "HTTPS_PROXY=http://custom:9000") {
		t.Fatalf("explicit HTTPS_PROXY not respected:\n%s", joined)
	}
	if !strings.Contains(joined, "GOPROXY=off") {
		t.Fatalf("explicit GOPROXY not respected:\n%s", joined)
	}
	if strings.Contains(joined, "HTTPS_PROXY=http://proxy:3128") {
		t.Fatalf("default HTTPS_PROXY overwrote explicit:\n%s", joined)
	}
}
