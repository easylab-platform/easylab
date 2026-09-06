package ops

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func writeExec(path, content string) error {
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		return err
	}
	return nil
}

// TestBuildahBuilderName verifies the buildah builder labels itself buildah.
func TestBuildahBuilderName(t *testing.T) {
	b := NewBuildahBuilder("buildah")
	if b.Name() != "buildah" {
		t.Fatalf("name: %s", b.Name())
	}
}

// TestBuildahRequiresExecutable ensures a missing buildah returns a clear error.
func TestBuildahRequiresExecutable(t *testing.T) {
	b := NewBuildahBuilder("/nonexistent/buildah")
	_, err := b.Build(context.Background(), BuildSpec{Context: t.TempDir(), Image: "x/image:latest"}, nil)
	if err == nil {
		t.Fatal("expected error for missing buildah")
	}
	if !strings.Contains(err.Error(), "buildah") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestBuildahCancel kills the build when the timeout elapses (fake buildah that sleeps).
func TestBuildahCancel(t *testing.T) {
	dir := t.TempDir()
	fake := dir + "/buildah"
	_ = writeExec(fake, "#!/bin/sh\nexec sh -c 'sleep 30'\n")
	b := NewBuildahBuilder(fake)
	start := time.Now()
	_, err := b.Build(context.Background(), BuildSpec{Context: dir, Image: "x/image:latest", Timeout: 200 * time.Millisecond}, nil)
	if err == nil {
		t.Fatal("expected timeout")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("did not kill promptly: %v", elapsed)
	}
}

// TestNewBuilderFromEnvDefaultBuildah verifies the default backend is buildah.
func TestNewBuilderFromEnvDefaultBuildah(t *testing.T) {
	t.Setenv("EASYVCS_BUILD_BACKEND", "")
	b := NewBuilderFromEnv()
	if b.Name() != "buildah" {
		t.Fatalf("default backend: %s", b.Name())
	}
}
