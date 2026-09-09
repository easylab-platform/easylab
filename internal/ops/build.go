package ops

import (
	"encoding/json"
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/build"
	imagepkg "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
)

// BuildSpec describes a container image build.
type BuildSpec struct {
	// Context is the build context directory (a checkout).
	Context string
	// Containerfile is the raw Dockerfile/Containerfile body used when Context
	// holds a writable scratch dir and the body must be materialized.
	Containerfile string
	// Dockerfile is a path to the Dockerfile relative to Context (default
	// "Dockerfile").
	Dockerfile string
	// Image is the destination image ref (e.g. "host/ns/app:v1").
	Image string
	// Registry is the address of the OCI registry to push to. Empty disables
	// push.
	Registry string
	// RegistryAuth is a basic-auth "user:pass" for the target registry.
	RegistryAuth string
	// CacheRepo / CacheKey are legacy cache-through knobs; the podman layer
	// cache is reused when building against the same base, so they are inert.
	CacheRepo string
	CacheKey  string
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

// Builder is the image-build execution seam. The backend is the podman
// sidecar's buildah (via docker/client ImageBuild).
type Builder interface {
	Build(ctx context.Context, spec BuildSpec, log func(string)) (BuildResult, error)
	Name() string
}

// podmanBuilder builds container images through the podman sidecar's
// buildah-in-podman, driven by the Docker-compatible API (docker/client). It is
// static (no CGO) and reuses the same OCI registry graph the sidecar owns.
type podmanBuilder struct {
	cli *client.Client
}

// NewPodmanBackendBuilder builds a Builder backed by the podman API.
func NewPodmanBackendBuilder(cli *client.Client) *podmanBuilder {
	return &podmanBuilder{cli: cli}
}

// Name implements Builder.
func (b *podmanBuilder) Name() string { return "podman" }

// NewBuilderFromEnv returns the image-build backend: podman's buildah (via the
// Docker-compatible API). DOCKER_HOST selects the sidecar socket.
func NewBuilderFromEnv() Builder {
	cli, err := client.NewClientWithOpts(
		client.WithHost(dockerHost()),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		panic(fmt.Sprintf("podman API client: %v", err))
	}
	return NewPodmanBackendBuilder(cli)
}

// buildDir returns the writable scratch root used for build contexts.
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

// packContextTar walks ctxDir into a tar stream (ImageBuild context). It
// returns the raw pipe reader: wrapping the stream in a *tar.Reader would
// buffer-ahead and truncate the body, so podman's buildah would read an empty
// context and fail with "stat .../Dockerfile: no such file".
func packContextTar(ctxDir string) (io.Reader, error) {
	var files []buildFile
	err := filepath.Walk(ctxDir, func(name string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(name, ctxDir)
		rel = strings.TrimPrefix(rel, "/")
		files = append(files, buildFile{name: rel, data: data})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tarStream(files), nil
}

type buildFile struct {
	name string
	data []byte
}

func tarStream(files []buildFile) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		tw := tar.NewWriter(pw)
		for _, f := range files {
			hdr := &tar.Header{Name: f.name, Mode: 0o644, Size: int64(len(f.data))}
			_ = tw.WriteHeader(hdr)
			_, _ = tw.Write(f.data)
		}
		_ = tw.Close()
		_ = pw.Close()
	}()
	return pr
}

// hostOf extracts scheme://host from a registry/ref as the auth key.
func hostOf(ref string) string {
	ref = strings.TrimPrefix(ref, "https://")
	ref = strings.TrimPrefix(ref, "http://")
	if i := strings.Index(ref, "/"); i > 0 {
		return ref[:i]
	}
	return ref
}

// Build implements Builder via podman's ImageBuild.
func (b *podmanBuilder) Build(ctx context.Context, spec BuildSpec, log func(string)) (BuildResult, error) {
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	if spec.Context == "" {
		return BuildResult{}, errors.New("build requires a context directory")
	}
	dockerfile := spec.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	if spec.Containerfile != "" {
		full := filepath.Join(spec.Context, dockerfile)
		if err := os.WriteFile(full, []byte(spec.Containerfile), 0o644); err != nil {
			return BuildResult{}, err
		}
	}

	ctxReader, err := packContextTar(spec.Context)
	if err != nil {
		return BuildResult{}, err
	}

	buildArgs := map[string]*string{}
	for _, ba := range spec.BuildArgs {
		k, v, _ := strings.Cut(ba, "=")
		val := v
		buildArgs[k] = &val
	}

	opts := build.ImageBuildOptions{
		Dockerfile: dockerfile,
		Tags:       []string{spec.Image},
		NoCache:    spec.NoCache,
		BuildArgs:  buildArgs,
		AuthConfigs: map[string]registry.AuthConfig{
			hostOf(spec.Registry): {Username: "", Password: ""},
		},
		Context: ctxReader,
	}
	if spec.RegistryAuth != "" && spec.Registry != "" {
		u, p, _ := strings.Cut(spec.RegistryAuth, ":")
		opts.AuthConfigs[hostOf(spec.Registry)] = registry.AuthConfig{Username: u, Password: p}
	}

	resp, err := b.cli.ImageBuild(ctx, ctxReader, opts)
	if err != nil {
		return BuildResult{}, err
	}
	defer resp.Body.Close()

	var out strings.Builder
	var buildErr string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		out.WriteString(line + "\n")
		if log != nil {
			log(line)
		}
		// The docker/podman build API reports RUN failures as JSON error
		// entries inside a 200-stream — the HTTP call itself succeeds. Without
		// this check every failed RUN (npm publish, cargo publish, ...) reads
		// as a green build.
		if e := streamBuildError(line); e != "" && buildErr == "" {
			buildErr = e
		}
	}
	if err := sc.Err(); err != nil {
		return BuildResult{}, err
	}
	if buildErr != "" {
		return BuildResult{Image: spec.Image, Out: out.String()}, fmt.Errorf("build failed: %s", buildErr)
	}

	// Push to the registry if one was given (podman buildah writes into the
	// sidecar's graph; push publishes to the OCI registry).
	if spec.Registry != "" && spec.Image != "" {
		pc, err := b.cli.ImagePush(ctx, spec.Image, imagepkg.PushOptions{RegistryAuth: ""})
		if err != nil {
			return BuildResult{Image: spec.Image, Out: out.String()}, err
		}
		defer pc.Close()
		sc2 := bufio.NewScanner(pc)
		for sc2.Scan() {
			line := sc2.Text()
			out.WriteString(line + "\n")
			if log != nil {
				log(line)
			}
		}
	}

	return BuildResult{Image: spec.Image, Out: out.String()}, nil
}

// streamBuildError extracts the error message from one build-stream line
// ({"error":"...","errorDetail":{"message":"..."}}), empty when the line is
// ordinary progress output.
func streamBuildError(line string) string {
	var ev struct {
		Error        string `json:"error"`
		ErrorDetail  struct {
			Message string `json:"message"`
		} `json:"errorDetail"`
	}
	if err := json.Unmarshal([]byte(line), &ev); err != nil {
		return ""
	}
	if ev.ErrorDetail.Message != "" {
		return ev.ErrorDetail.Message
	}
	return ev.Error
}
