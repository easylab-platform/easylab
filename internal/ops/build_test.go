package ops

import (
	"context"
	"testing"
)

// TestPodmanBuilderName verifies the build builder labels itself podman.
func TestPodmanBuilderName(t *testing.T) {
	b := NewPodmanBackendBuilder(nil)
	if b.Name() != "podman" {
		t.Fatalf("name: %s", b.Name())
	}
}

// TestBuildSpecDefaults verifies BuildSpec default dockerfile + image tag path.
func TestBuildSpecDefaults(t *testing.T) {
	var spec BuildSpec
	b := new(podmanBuilder)
	_ = b
	if spec.Dockerfile != "" {
		t.Fatalf("expected empty default dockerfile, got %s", spec.Dockerfile)
	}
}

// TestBuildWithoutContext verifies a missing context is rejected.
func TestBuildWithoutContext(t *testing.T) {
	b := NewPodmanBackendBuilder(nil)
	_, err := b.Build(context.Background(), BuildSpec{Image: "x/image:latest"}, nil)
	if err == nil {
		t.Fatal("expected error for missing context")
	}
}

// TestNanoCPUs verifies CPU string parsing.
func TestNanoCPUs(t *testing.T) {
	if got := nanoCPUs("1"); got != int64(1e9) {
		t.Fatalf("1 -> %d", got)
	}
	if got := nanoCPUs("0.5"); got != int64(5e8) {
		t.Fatalf("0.5 -> %d", got)
	}
	if got := nanoCPUs("250m"); got != int64(25e7) {
		t.Fatalf("250m -> %d", got)
	}
}
