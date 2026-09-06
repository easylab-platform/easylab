package ops

import (
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
