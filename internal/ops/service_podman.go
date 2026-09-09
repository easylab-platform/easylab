package ops

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	imagepkg "github.com/docker/docker/api/types/image"
	networkapi "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/registry"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"
)

// PodmanServiceRunner drives service containers through the podman privileged
// sidecar using the official Docker-compatible API (docker/client). It does NOT
// exec a local podman CLI and does NOT require CGO — the easylab binary stays
// static. It dials the podman API over DOCKER_HOST (default the shared sidecar
// socket: unix:///run/podman/podman.sock). Service networks, cgroup limits and
// port publishing map onto the podman API.
//
// The podman sidecar runs `podman system service` (Docker-compatible), so image
// builds go to podman's buildah (ImageBuild) and every build/container lands in
// the same OCI registry graph the sidecar owns (EASYVCS_REGISTRY).
type PodmanServiceRunner struct {
	registryHost string
	// selfBase is the externally-reachable easylab base URL (from
	// EASYVCS_SELF_BASE). Service containers live on their own bridge network,
	// so they must reach easylab by this domain (not loopback) — used to build
	// the per-tool registry/proxy env injected into launched containers.
	selfBase string
	// upstreamProxy is the HTTP(S) proxy service containers and easylab use for
	// general outbound traffic (from EASYLAB_UPSTREAM_PROXY). Empty disables
	// injection.
	upstreamProxy string
	workRoot      string
	cli           *client.Client
}

// dockerHost returns the podman API socket (or override).
func dockerHost() string {
	if v := os.Getenv("DOCKER_HOST"); v != "" {
		return v
	}
	if v := os.Getenv("EASYLAB_PODMAN_URI"); v != "" {
		return v
	}
	return "unix:///run/podman/podman.sock"
}

// NewPodmanServiceRunner builds a docker/client-backed podman runner.
func NewPodmanServiceRunner(registryHost, workRoot string) *PodmanServiceRunner {
	return NewPodmanServiceRunnerWithProxy(registryHost, workRoot,
		os.Getenv("EASYVCS_SELF_BASE"), os.Getenv("EASYLAB_UPSTREAM_PROXY"))
}

// NewPodmanServiceRunnerWithProxy builds a runner with explicit self-base and
// upstream proxy addresses (used to inject registry/proxy env into launched
// service containers).
func NewPodmanServiceRunnerWithProxy(registryHost, workRoot, selfBase, upstreamProxy string) *PodmanServiceRunner {
	cli, err := client.NewClientWithOpts(
		client.WithHost(dockerHost()),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		panic(fmt.Sprintf("podman API client: %v", err))
	}
	return &PodmanServiceRunner{
		registryHost:  registryHost,
		selfBase:      strings.TrimSuffix(selfBase, "/"),
		upstreamProxy: upstreamProxy,
		workRoot:      workRoot,
		cli:           cli,
	}
}

// Name implements ServiceRunner.
func (r *PodmanServiceRunner) Name() string { return "podman" }

// getRegistryAuth builds an auth config for the internal registry (bare
// host/repo). Pulling from the built-in /v2 is open (no auth); but when a
// registry auth is configured it is honored. We pass empty auth which works
// for the open in-cluster registry.
func (r *PodmanServiceRunner) authFor(image string) registry.AuthConfig {
	return registry.AuthConfig{}
}

// qualify prepends the internal registry host to a bare image name, mirroring
// the CLI heuristic (so unqualified "alpine" resolves to the internal OCI).
func (r *PodmanServiceRunner) qualify(image string) string {
	if strings.Contains(image, "/") && !strings.Contains(strings.SplitN(image, "/", 2)[0], ".") &&
		!strings.Contains(strings.SplitN(image, "/", 2)[0], ":") {
		return r.registryHost + "/" + image
	}
	if !strings.Contains(image, ":") && !strings.Contains(image, "/") {
		return r.registryHost + "/library/" + image + ":latest"
	}
	return image
}

// ensureNetwork creates a bridge network for name (Docker-compatible; podman
// maps this to its netavark bridge + embedded DNS).
func (r *PodmanServiceRunner) ensureNetwork(ctx context.Context, name string) error {
	if name == "" {
		return nil
	}
	// Exists?
	if _, err := r.cli.NetworkInspect(ctx, name, networkapi.InspectOptions{}); err == nil {
		return nil
	}
	_, err := r.cli.NetworkCreate(ctx, name, networkapi.CreateOptions{Driver: "bridge"})
	return err
}

// Exec runs a command inside a running service container via the exec API and
// returns its combined output.
func (r *PodmanServiceRunner) Exec(ctx context.Context, name string, command string) (string, error) {
	if command == "" {
		return "", fmt.Errorf("exec requires a command")
	}
	cfg := container.ExecOptions{
		Cmd:          []string{"sh", "-c", command},
		AttachStdout: true,
		AttachStderr: true,
	}
	execID, err := r.cli.ContainerExecCreate(ctx, name, cfg)
	if err != nil {
		return "", err
	}
	resp, err := r.cli.ContainerExecAttach(ctx, execID.ID, container.ExecAttachOptions{})
	if err != nil {
		return "", err
	}
	defer resp.Close()
	var buf bytes.Buffer
	_, _ = stdcopy.StdCopy(&buf, &buf, resp.Reader)
	return strings.TrimSpace(buf.String()), nil
}

// ContainerFile writes content to a path inside a running container via
// CopyToContainer (tar stream).
func (r *PodmanServiceRunner) ContainerFile(ctx context.Context, name, path string, content []byte) error {
	tarStream := tarBytes([]tarEntry{{name: filepath.Base(path), data: content}})
	return r.cli.CopyToContainer(ctx, name, filepath.Dir(path), tarStream, container.CopyToContainerOptions{})
}

// ReadContainerFile copies a path out of a container via CopyFromContainer.
func (r *PodmanServiceRunner) ReadContainerFile(ctx context.Context, name, path string) ([]byte, error) {
	rc, _, err := r.cli.CopyFromContainer(ctx, name, path)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	br := tar.NewReader(rc)
	for {
		hdr, err := br.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("no file at %s", path)
		}
		if err != nil {
			return nil, err
		}
		if hdr.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(br)
			if err != nil {
				return nil, err
			}
			return data, nil
		}
	}
}

// netName returns the network name for a service.
func (r *PodmanServiceRunner) netName(name string) string { return name }

// containerIP returns the container's IP on its network.
func (r *PodmanServiceRunner) containerIP(ctx context.Context, name string) (string, error) {
	insp, err := r.cli.ContainerInspect(ctx, name)
	if err != nil {
		return "", err
	}
	if insp.NetworkSettings == nil {
		return "", nil
	}
	return insp.NetworkSettings.IPAddress, nil
}

// injectedEnv returns the env for a launched service container: the caller's
// explicit req.Env takes precedence, and any missing registry/proxy knobs are
// filled in so package managers pull through easylab and general outbound
// traffic goes through the configured upstream proxy — without the container
// needing any configuration.
//
// Explicit entries never get overwritten, so a caller can override any knob
// per launch (ServiceRequest.Env), giving per-container customization.
func (r *PodmanServiceRunner) injectedEnv(explicit []string) []string {
	exists := map[string]bool{}
	for _, e := range explicit {
		if i := strings.Index(e, "="); i > 0 {
			exists[e[:i]] = true
		}
	}
	add := func(k, v string) []string {
		if v != "" && !exists[k] {
			exists[k] = true
			return append(explicit, k+"="+v)
		}
		return explicit
	}

	base := r.selfBase
	if base == "" {
		base = "http://127.0.0.1:8080"
	}
	base = strings.TrimSuffix(base, "/")
	noProxy := "127.0.0.1,localhost,.svc.cluster.local,.svc"
	if h := hostOf(base); h != "" {
		noProxy += "," + h
	}
	// General outbound: npm/pip/go/cargo are package registries handled below;
	// everything else (github, generic HTTP) goes through the upstream proxy.
	if r.upstreamProxy != "" {
		explicit = add("HTTP_PROXY", r.upstreamProxy)
		explicit = add("HTTPS_PROXY", r.upstreamProxy)
		explicit = add("http_proxy", r.upstreamProxy)
		explicit = add("https_proxy", r.upstreamProxy)
		explicit = add("NO_PROXY", noProxy)
		explicit = add("no_proxy", noProxy)
	} else {
		explicit = add("NO_PROXY", noProxy)
		explicit = add("no_proxy", noProxy)
	}

	// Package registries: pull-through easylab so package downloads reuse the
	// internal cache instead of reaching the public upstream. The tool-specific
	// env var is honored by the client so no per-container config is needed.
	explicit = add("NPM_CONFIG_REGISTRY", base+"/pkgs/npm")
	explicit = add("PIP_INDEX_URL", base+"/pkgs/pypi/simple")
	explicit = add("GOPROXY", base+"/pkgs/go")
	explicit = add("GOSUMDB", "off")
	// cargo index override (index is a bare crates-io mirror); fall back to the
	// GitHub-index form which crates is also happy to talk to.
	explicit = add("CARGO_REGISTRIES_CRATES_IO_INDEX", base+"/pkgs/cargo")
	return explicit
}

// Launch creates the network, pulls the image, and starts the requested number
// of containers with cgroup resource limits.
func (r *PodmanServiceRunner) Launch(ctx context.Context, req ServiceRequest, log func(string)) (ServiceStatus, error) {
	if req.Name == "" {
		return ServiceStatus{}, fmt.Errorf("service name required")
	}
	if req.Image == "" {
		return ServiceStatus{}, fmt.Errorf("service image required")
	}
	net := req.NetworkName()
	if err := r.ensureNetwork(ctx, net); err != nil {
		return ServiceStatus{}, fmt.Errorf("create network: %w", err)
	}
	image := r.qualify(req.Image)
	// Pull if absent (Docker-compatible pull); ignore not-found-on-inspect.
	if _, _, err := r.cli.ImageInspectWithRaw(ctx, image); err != nil {
		if _, perr := r.cli.ImagePull(ctx, image, imagepkg.PullOptions{}); perr != nil {
			return ServiceStatus{}, fmt.Errorf("pull %s: %w", image, perr)
		}
		if log != nil {
			log("pulled " + image)
		}
	}
	reps := req.Replicas
	if reps <= 0 {
		reps = 1
	}
	restart := req.Restart
	if restart == "" {
		restart = "always"
	}

	var ids []string
	firstIP := ""
	for i := 0; i < reps; i++ {
		containerName := req.Name
		if reps > 1 {
			containerName = fmt.Sprintf("%s-%d", req.Name, i)
		}
		binds := []string{}
		exposed := map[nat.Port]struct{}{}
		pbinds := nat.PortMap{}
		for c, h := range req.Ports {
			port := nat.Port(fmt.Sprintf("%d/tcp", c))
			exposed[port] = struct{}{}
			switch {
			case h > 0: // explicit loopback publish
				pbinds[port] = []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: fmt.Sprintf("%d", h)}}
			case h == -1: // auto-assigned loopback publish (sandboxes)
				pbinds[port] = []nat.PortBinding{{HostIP: "127.0.0.1", HostPort: ""}}
			}
		}
		hostCfg := &container.HostConfig{
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyMode(restart)},
			NetworkMode:   container.NetworkMode(net),
			PortBindings:  pbinds,
			Binds:         binds,
			Resources:     container.Resources{},
		}
		if req.CPUs != "" {
			hostCfg.Resources.NanoCPUs = nanoCPUs(req.CPUs)
		}
		if req.MemoryBytes > 0 {
			hostCfg.Resources.Memory = int64(req.MemoryBytes)
		}
		env, cmd := r.injectedEnv(req.Env), []string{}
		if req.Command != "" {
			cmd = []string{"sh", "-c", req.Command}
		}
		labels := req.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		cfgspec := &container.Config{
			Image:        image,
			Cmd:          cmd,
			Env:          env,
			Labels:       labels,
			ExposedPorts: exposed,
		}
		created, err := r.cli.ContainerCreate(ctx, cfgspec, hostCfg, nil, nil, containerName)
		if err != nil {
			return ServiceStatus{}, fmt.Errorf("create %s: %w", containerName, err)
		}
		if err := r.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
			return ServiceStatus{}, fmt.Errorf("start %s: %w", containerName, err)
		}
		ids = append(ids, created.ID)
		if log != nil {
			log("launch " + containerName)
		}
		if firstIP == "" {
			firstIP, _ = r.containerIP(ctx, containerName)
		}
	}
	primary := req.Name
	if reps > 1 {
		primary = req.Name + "-0"
	}
	return r.statusOf(ctx, primary, req.Name, reps, ids, firstIP, req.Ports, net), nil
}

// statusOf assembles a ServiceStatus.
func (r *PodmanServiceRunner) statusOf(ctx context.Context, primary, name string, reps int, ids []string, ip string, ports map[int]int, net string) ServiceStatus {
	st := ServiceStatus{
		Name:         name,
		Kind:         "deployment",
		Replicas:     reps,
		Ready:        reps,
		Phase:        "Running",
		PodIP:        ip,
		ContainerIDs: ids,
		ServiceURL:   name,
		Network:      net,
	}
	if h := firstHostPort(ports); h != 0 {
		st.WorkerURL = fmt.Sprintf("http://127.0.0.1:%d", h)
	}
	return st
}

// Status inspects a single container by name (or id).
func (r *PodmanServiceRunner) Status(ctx context.Context, name string) (ServiceStatus, error) {
	insp, err := r.cli.ContainerInspect(ctx, name)
	if err != nil {
		if client.IsErrNotFound(err) {
			return ServiceStatus{}, os.ErrNotExist
		}
		return ServiceStatus{}, err
	}
	ip, _ := r.containerIP(ctx, insp.ID)
	state := insp.State.Status
	st := ServiceStatus{Name: name, Kind: "deployment", Phase: state, Replicas: 1, Ready: 1, PodIP: ip, ServiceURL: name}
	if state == "exited" || state == "dead" || state == "created" {
		st.Ready = 0
	}
	return st, nil
}

// Delete removes a service's containers (by name prefix) and network.
func (r *PodmanServiceRunner) Delete(ctx context.Context, name string) error {
	candidates := []string{name}
	for i := 0; i < 64; i++ {
		candidates = append(candidates, fmt.Sprintf("%s-%d", name, i))
	}
	for _, c := range candidates {
		insp, err := r.cli.ContainerInspect(ctx, c)
		if err != nil {
			continue
		}
		_ = r.cli.ContainerStop(ctx, insp.ID, container.StopOptions{})
		_ = r.cli.ContainerRemove(ctx, insp.ID, container.RemoveOptions{Force: true})
	}
	_ = r.cli.NetworkRemove(ctx, r.netName(name))
	return nil
}

// Scale adjusts replicas by starting/stopping <name>-<N> containers.
func (r *PodmanServiceRunner) Scale(ctx context.Context, name string, replicas int) (ServiceStatus, error) {
	current := 1
	for i := 1; ; i++ {
		c := fmt.Sprintf("%s-%d", name, i)
		if _, err := r.cli.ContainerInspect(ctx, c); err == nil {
			current = i + 1
		} else {
			break
		}
	}
	for i := current; i < replicas; i++ {
		c := fmt.Sprintf("%s-%d", name, i)
		if insp, err := r.cli.ContainerInspect(ctx, c); err == nil {
			_ = r.cli.ContainerStart(ctx, insp.ID, container.StartOptions{})
		}
	}
	for i := replicas; i < current; i++ {
		c := fmt.Sprintf("%s-%d", name, i)
		if insp, err := r.cli.ContainerInspect(ctx, c); err == nil {
			_ = r.cli.ContainerStop(ctx, insp.ID, container.StopOptions{})
		}
	}
	ip, _ := r.containerIP(ctx, name)
	return ServiceStatus{Name: name, Kind: "deployment", Replicas: replicas, Ready: replicas, PodIP: ip, ServiceURL: name}, nil
}

// List lists containers, optionally filtered by network.
func (r *PodmanServiceRunner) List(ctx context.Context, network string) ([]ServiceStatus, error) {
	opts := container.ListOptions{All: true}
	if network != "" {
		opts.Filters = filters.NewArgs(filters.Arg("network", network))
	}
	summaries, err := r.cli.ContainerList(ctx, opts)
	if err != nil {
		return nil, err
	}
	var list []ServiceStatus
	for _, s := range summaries {
		st := ServiceStatus{Name: s.Names[0], Kind: "deployment", Replicas: 1}
		if ip, e := r.containerIP(ctx, s.ID); e == nil {
			st.PodIP = ip
		}
		list = append(list, st)
	}
	return list, nil
}

// SyncTar extracts a tarball into a running service container at dest.
func (r *PodmanServiceRunner) SyncTar(ctx context.Context, name, dest string, tarball []byte) error {
	if dest == "" {
		dest = "/workspace"
	}
	return r.cli.CopyToContainer(ctx, name, dest, bytes.NewReader(tarball), container.CopyToContainerOptions{})
}

var _ ServiceRunner = (*PodmanServiceRunner)(nil)

// ---- helpers ----

func nanoCPUs(cpus string) int64 {
	// Accept "1", "0.5", "250m"; convert to nano-CPUs.
	var v float64
	if strings.HasSuffix(cpus, "m") {
		fmt.Sscanf(cpus[:len(cpus)-1], "%f", &v)
		v = v / 1000
	} else {
		fmt.Sscanf(cpus, "%f", &v)
	}
	return int64(v * 1e9)
}

type tarEntry struct {
	name string
	data []byte
}

func tarBytes(entries []tarEntry) io.Reader {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.data))}
		_ = tw.WriteHeader(hdr)
		_, _ = tw.Write(e.data)
	}
	_ = tw.Close()
	return bytes.NewReader(buf.Bytes())
}

var _ = json.Marshal
var _ = http.StatusOK
var _ = time.Second
