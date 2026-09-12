package k8s

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// BuildOptions describes one image build.
type BuildOptions struct {
	// ContextDir is easylab's in-container build context dir (under /data),
	// holding the repo tree + Dockerfile.
	ContextDir string
	Dockerfile string // relative to ContextDir; default "Dockerfile"
	Image      string // destination ref, pushed to easylab's OCI registry
	BuildArgs  map[string]string
	NoCache    bool
	// Timeout bounds the build (0 => 30m).
	Timeout time.Duration
}

// BuildImage builds + pushes an image using the unified job primitive. The
// image is the easylab buildkit-worker (buildkitd + easyworker in one
// container); easylab launches it as a normal worker job, syncs the build
// context into it and runs `buildctl` through the worker API — no sidecar, no
// shared socket volume, no hostPath meta files. Cache is registry-backed
// (type=registry) so it is shared across ephemeral build pods and survives
// pod recreation.
func (c *Client) BuildImage(ctx context.Context, opt BuildOptions, logf func(string)) error {
	if opt.Image == "" {
		return fmt.Errorf("build image required")
	}
	ctxDir := opt.ContextDir
	if ctxDir == "" {
		return fmt.Errorf("build context required")
	}
	tarball, err := tarDir(ctxDir)
	if err != nil {
		return fmt.Errorf("tar build context: %w", err)
	}
	filename := opt.Dockerfile
	if filename == "" {
		filename = "Dockerfile"
	}
	token, err := c.randomToken()
	if err != nil {
		return err
	}

	env := map[string]string{
		"EASYLAB_REGISTRY_HOST":  c.registryHost,
		"EASYLAB_REGISTRY_TOKEN": c.registryToken,
	}
	for k, v := range c.proxyVars() {
		env[k] = v
	}

	name := fmt.Sprintf("easylab-build-%d", time.Now().UnixNano())
	spec := JobSpec{
		Name:      name,
		Image:     c.qualifyRuntimeImage(c.buildkitImage),
		Workspace: "/workspace",
		Env:       env,
		Tarball:   tarball,
		Commands:  []JobCommand{{Name: "build", Run: buildctlCommand(c, opt, filename), Workdir: "/workspace"}},
		Timeout:   opt.Timeout,
		Profile: &RuntimeProfile{
			Name:         "buildkit",
			Derived:      false,
			WorkerPort:   48080,
			ReadyTimeout: "2m",
			// Match the cluster buildkitd posture: rootless uid/gid 1000 with
			// SYS_ADMIN and unconfined seccomp/apparmor. RunAsGroup and
			// appArmorProfile are required for rootlesskit to set up its mount
			// namespace, exactly as the shared buildkitd StatefulSet does.
			SecurityContext: &corev1.SecurityContext{
				RunAsUser:                int64Ptr(1000),
				RunAsGroup:               int64Ptr(1000),
				RunAsNonRoot:             boolPtr(true),
				AllowPrivilegeEscalation: boolPtr(true),
				Capabilities:             &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}},
				AppArmorProfile:          &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
				SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
			},
		},
	}
	res, err := c.RunJob(ctx, spec, token, logf)
	if err != nil {
		return fmt.Errorf("buildkit build failed: %w", err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("buildkit build failed (exit %d)", res.ExitCode)
	}
	return nil
}

// buildctlCommand assembles the buildctl invocation the worker runs against
// its local buildkitd. The worker's builtin shell runs an allowlisted env, so
// the daemon address is passed explicitly (not via BUILDKIT_HOST). Progress is
// merged to stdout (2>&1) so WatchJob streams the whole build live.
func buildctlCommand(c *Client, opt BuildOptions, filename string) string {
	args := []string{
		"buildctl --addr unix:///run/user/1000/buildkit/buildkitd.sock build",
		"--frontend dockerfile.v0",
		"--local context=/workspace",
		"--local dockerfile=/workspace",
		"--opt filename=" + filename,
	}
	for k, v := range opt.BuildArgs {
		args = append(args, "--opt build-arg:"+k+"="+v)
	}
	if opt.NoCache {
		args = append(args, "--no-cache")
	}
	if ref := c.cacheRefFor(opt.Image); ref != "" {
		args = append(args, "--export-cache type=registry,ref="+ref+",mode=max")
		args = append(args, "--import-cache type=registry,ref="+ref)
	}
	args = append(args, "--output type=image,name="+opt.Image+",push=true")
	args = append(args, "--progress plain")
	return strings.Join(args, " ") + " 2>&1"
}

// tarDir packages a directory tree (files + dirs) as an uncompressed tar. The
// paths are relative to dir (the worker unpacks at the workspace root).
func tarDir(dir string) ([]byte, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, cerr := io.Copy(tw, f)
			f.Close()
			if cerr != nil {
				return cerr
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
