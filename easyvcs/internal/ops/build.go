package ops

import (
	"bufio"
	"context"
	"encoding/base64"
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

// killGroup sends SIGKILL to the process group led by pid, mirroring the
// process-group isolation used by the local runtime so a timeout tears down
// the whole build (and any subprocesses).
func killGroup(pid int) error {
	if pid <= 0 {
		return nil
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// BuildSpec describes a container image build.
type BuildSpec struct {
	// Context is the build context directory (a checkout), or empty to build
	// from a raw Containerfile.
	Context string
	// Containerfile is the raw Dockerfile/Containerfile body used when Context
	// is empty.
	Containerfile string
	// Dockerfile is a path to the Dockerfile relative to Context (default
	// "Containerfile").
	Dockerfile string
	// Image is the destination image ref (e.g. "registry.example.com/a/app:v1").
	Image string
	// Registry is the address of the OCI registry to push to. Empty disables
	// push.
	Registry string
	// RegistryAuth is a basic-auth "user:pass" for the target registry, when
	// auth is on. It is scoped to a temporary docker config so it never leaks.
	RegistryAuth string
	// CacheRepo is a repository (in Registry) used to export/import build
	// cache. When set, the build seeds from (--cache-from) and writes back
	// (--cache-to) through the registry, so the OCI registry holds reusable
	// layers shared across builds.
	CacheRepo string
	// CacheKey is an optional opaque key suffix scoping the cache (e.g. a
	// platform/arch or a CI run). Defaults to "_builtin".
	CacheKey string
	// BuildArgs are --build-arg KEY=VAL entries.
	BuildArgs []string
	// NoCache disables layer caching.
	NoCache bool
	// Timeout bounds the build (0 = no limit).
	Timeout time.Duration
}

// BuildResult is the outcome of an image build.
type BuildResult struct {
	Image string // final image ref (when pushed)
	Out   string // build log tail
}

// Builder is the image-build execution seam.
type Builder interface {
	Build(ctx context.Context, spec BuildSpec, log func(string)) (BuildResult, error)
	Name() string
}

func writeDockerConfig(registry, auth string) (string, error) {
	if registry == "" || auth == "" {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "easyvcs-build-auth")
	if err != nil {
		return "", err
	}
	dockerDir := filepath.Join(dir, ".docker")
	if err := os.MkdirAll(dockerDir, 0o700); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	encoded := encodeBasicAuth(auth)
	cfg := fmt.Sprintf(`{"auths":{"%s":{"auth":"%s"}}}`, registry, encoded)
	if err := os.WriteFile(filepath.Join(dockerDir, "config.json"), []byte(cfg), 0o600); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

func encodeBasicAuth(auth string) string {
	return base64.StdEncoding.EncodeToString([]byte(auth))
}

// buildahBuilder builds container images with the daemonless, rootless
// `buildah` client. It needs no buildkitd and no /run write access, so it is
// fully self-contained inside the EasyLab container. It builds with --layers
// for local layer caching and, when CacheRepo is set, seeds/writes the cache
// through the OCI registry so layers are shared across builds.
type buildahBuilder struct {
	bin string // path to buildah
}

// NewBuildahBuilder builds a Builder backed by buildah at bin.
func NewBuildahBuilder(bin string) *buildahBuilder {
	if bin == "" {
		bin = "buildah"
	}
	return &buildahBuilder{bin: bin}
}

// Name implements Builder.
func (b *buildahBuilder) Name() string { return "buildah" }

// NewBuilderFromEnv returns the sole image-build backend: an embedded buildah
// (daemonless, rootless). EasyLab only supports in-container builds; there is
// no buildctl/buildkitd backend. EASYVCS_BUILDAH_BIN overrides the binary path.
func NewBuilderFromEnv() Builder {
	bin := os.Getenv("EASYVCS_BUILDAH_BIN")
	if bin == "" {
		bin = "buildah"
	}
	return NewBuildahBuilder(bin)
}

// resolveBin locates the buildah executable, preferring the configured path
// then PATH.
func (b *buildahBuilder) resolveBin() (string, error) {
	if _, err := os.Stat(b.bin); err == nil {
		return b.bin, nil
	}
	if p, err := exec.LookPath(b.bin); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("buildah not found (tried %s)", b.bin)
}

// Build implements Builder via `buildah bud`. It builds with --layers (local
// cache), and when CacheRepo is set uses --cache-from/--cache-to through the
// OCI registry so layers are reused, then pushes to the registry if given.
// buildDir returns a writable ext4 scratch directory for buildah's context
// overlay scaffolding and TMPDIR. buildah (bud) mounts an overlay over the
// build context; that requires a writable non-tmpfs backing (e.g. an ext4
// hostPath/PVC). The default is <EASYVCS_HOME>/buildtmp; override with
// EASYVCS_BUILDAH_TMP. This mirrors the required on-host path.
func buildDir() string {
	if v := os.Getenv("EASYVCS_BUILDAH_TMP"); v != "" {
		return v
	}
	home := os.Getenv("EASYVCS_HOME")
	if home == "" {
		home = "/data"
	}
	return filepath.Join(home, "buildtmp")
}

func (b *buildahBuilder) Build(ctx context.Context, spec BuildSpec, log func(string)) (BuildResult, error) {
	bin, err := b.resolveBin()
	if err != nil {
		return BuildResult{}, err
	}

	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}

	// Working root for buildah: must be a writable ext4 dir so the context
	// overlay scaffolding can mount. Create it up front.
	workRoot := buildDir()
	if err := os.MkdirAll(workRoot, 0o755); err != nil {
		return BuildResult{}, err
	}

	// Resolve a build context directory.
	var ctxDir string
	if spec.Context != "" {
		ctxDir = spec.Context
	} else if spec.Containerfile != "" {
		d, err := scratchDir(workRoot, "easyvcs-build-ctx")
		if err != nil {
			return BuildResult{}, err
		}
		ctxDir = d
	} else {
		return BuildResult{}, fmt.Errorf("build requires a context or a raw Containerfile")
	}

	dockerfile := spec.Dockerfile
	if dockerfile == "" {
		dockerfile = "Containerfile"
	}

	// If a raw Containerfile body is supplied, materialise it into the context
	// directory under the dockerfile name so buildah picks it up.
	if spec.Containerfile != "" {
		full := filepath.Join(ctxDir, dockerfile)
		if err := os.WriteFile(full, []byte(spec.Containerfile), 0o644); err != nil {
			return BuildResult{}, err
		}
	}

	image := spec.Image
	args := []string{"bud"}
	// In-cluster registries are plain-HTTP / self-signed (the built-in /v2,
	// forgejo nip.io, etc.). Disable TLS verification so buildah can pull base
	// images, write the cache, and push the result to them.
	args = append(args, "--tls-verify=false")
	if image != "" {
		args = append(args, "-t", image)
	}
	if spec.NoCache {
		args = append(args, "--no-cache")
	} else {
		args = append(args, "--layers")
	}
	args = append(args, "-f", dockerfile)
	for _, ba := range spec.BuildArgs {
		args = append(args, "--build-arg", ba)
	}
	// Cache sharing through the OCI registry (seed + write-back), when set.
	// buildah uses plain image-reference form (host/repo[:tag]): --cache-from
	// takes a repo (no tag) and --cache-to takes a repo (no tag); they use the
	// buildah-local layer cache seeded from / written to the registry after the
	// build. Unlike buildkit, there is no `type=registry,ref=` wrapper.
	cacheRepo := spec.CacheRepo
	if cacheRepo != "" && !spec.NoCache {
		ref := cacheRepo
		args = append(args, "--cache-from", ref)
		args = append(args, "--cache-to", ref)
		args = append(args, "--cache-ttl", "168h")
	}
	args = append(args, ctxDir)

	// Isolate credentials for the target registry (scoped docker config), so
	// the build can push to /v2 (or a private base) without leaking tokens.
	authDir, err := writeDockerConfig(spec.Registry, spec.RegistryAuth)
	if err != nil {
		return BuildResult{}, err
	}
	if authDir != "" {
		defer os.RemoveAll(authDir)
	}

	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = ctxDir
	cmd.Env = os.Environ()
	// bud mounts an overlay over the build context; point TMPDIR at the
	// writable ext4 work root and force a no-daemon, rootless-friendly build.
	cmd.Env = append(cmd.Env,
		"TMPDIR="+workRoot,
		"BUILDAH_ISOLATION=chroot",
		"STORAGE_DRIVER=vfs",
	)
	if authDir != "" {
		cmd.Env = append(cmd.Env, "HOME="+authDir)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 500 * time.Millisecond
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killGroup(cmd.Process.Pid)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return BuildResult{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return BuildResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return BuildResult{}, err
	}

	var out strings.Builder
	scan := func(rd io.Reader) {
		sc := bufio.NewScanner(rd)
		for sc.Scan() {
			line := sc.Text()
			out.WriteString(line + "\n")
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
	if ctx.Err() != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return BuildResult{Image: image, Out: out.String()}, fmt.Errorf("build timed out")
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return BuildResult{Image: image, Out: out.String()}, fmt.Errorf("buildah exited %d", ee.ExitCode())
		}
		return BuildResult{Image: image, Out: out.String()}, err
	}

	// If a registry was given, push the built image to it.
	if image != "" && spec.Registry != "" {
		if err := pushBuildah(bin, image, spec.Registry, spec.RegistryAuth, ctx, log); err != nil {
			return BuildResult{Image: image, Out: out.String()}, err
		}
	}
	return BuildResult{Image: image, Out: out.String()}, nil
}

// pushBuildah runs `buildah push <image> docker://<image>`. The image is
// already a fully-qualified reference (e.g. host/ns/name:tag) because buildah
// bud tagged it with -t that name; do NOT prepend the registry again, which
// would double-prefix it into an invalid reference.
func pushBuildah(bin, image, registry, auth string, ctx context.Context, log func(string)) error {
	_ = registry
	authDir, err := writeDockerConfig(registry, auth)
	if err != nil {
		return err
	}
	if authDir != "" {
		defer os.RemoveAll(authDir)
	}
	ref := "docker://" + image
	cmd := exec.CommandContext(ctx, bin, "push", "--tls-verify=false", image, ref)
	cmd.Env = os.Environ()
	if authDir != "" {
		cmd.Env = append(cmd.Env, "HOME="+authDir)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return killGroup(cmd.Process.Pid)
	}
	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" && log != nil {
			log(line)
		}
	}
	if err != nil {
		return fmt.Errorf("buildah push failed: %w", err)
	}
	return nil
}
