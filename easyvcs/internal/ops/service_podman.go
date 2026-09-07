package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// PodmanServiceRunner launches services as podman containers running INSIDE the
// EasyLab container. It is the fully self-contained "internal podman" backend,
// chosen when EasyLab is run as a privileged pod with writable cgroups:
//
//   - per-service podman network (--network <name>, bridge + built-in DNS) so
//     services resolve each other by name (`http://<svc>:<port>`);
//   - real cgroup resource limits (--cpus / --memory) — requires the pod to
//     mount /sys/fs/cgroup read-write (privileged + a remount at startup);
//   - port publishing on the EasyLab loopback so a reverse proxy can reach
//     each service.
type PodmanServiceRunner struct {
	registryHost string
	workRoot     string // writable ext4 dir for podman storage/run
	bin          string
}

// NewPodmanServiceRunner builds a podman-backed runner.
func NewPodmanServiceRunner(registryHost, workRoot string) *PodmanServiceRunner {
	return &PodmanServiceRunner{
		registryHost: registryHost,
		workRoot:     workRoot,
		bin:          envOr("EASYVCS_PODMAN_BIN", "podman"),
	}
}

// Name implements ServiceRunner.
func (r *PodmanServiceRunner) Name() string { return "podman" }

// podmanEnv returns the env so podman uses our storage/run (not /var/lib, which
// is unwritable in a restricted pod). This is rootful podman; the pod is
// privileged with a rw cgroup mount.
func (r *PodmanServiceRunner) podmanEnv() []string {
	return []string{
		"STORAGE_DRIVER=vfs",
		"HOME=" + r.workRoot,
		"XDG_RUNTIME_DIR=" + r.workRoot,
	}
}

// runPodman executes a podman subcommand with our env.
func (r *PodmanServiceRunner) runPodman(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Env = append(os.Environ(), r.podmanEnv()...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s == "" {
			s = err.Error()
		}
		return s, fmt.Errorf("podman %s: %s", strings.Join(args, " "), s)
	}
	return s, nil
}

// Exec runs a command inside a running service container (podman exec) and
// returns its combined output. It is used by the sandbox pass-through.
func (r *PodmanServiceRunner) Exec(ctx context.Context, name string, command string) (string, error) {
	if command == "" {
		return "", fmt.Errorf("exec requires a command")
	}
	return r.runPodman(ctx, "exec", name, "sh", "-c", command)
}

// ContainerFile writes content to a path inside a running container via
// podman cp (temp file -> container).
func (r *PodmanServiceRunner) ContainerFile(ctx context.Context, name, path string, content []byte) error {
	tmpHost := filepath.Join(r.workRoot, fmt.Sprintf("containerfile-%d", time.Now().UnixNano()))
	if err := os.WriteFile(tmpHost, content, 0o600); err != nil {
		return err
	}
	defer os.Remove(tmpHost)
	_, err := r.runPodman(ctx, "cp", tmpHost, name+":"+path)
	return err
}

// ReadContainerFile copies a path out of a container to a temp file and returns
// its bytes.
func (r *PodmanServiceRunner) ReadContainerFile(ctx context.Context, name, path string) ([]byte, error) {
	tmpHost := filepath.Join(r.workRoot, fmt.Sprintf("read-%d", time.Now().UnixNano()))
	defer os.Remove(tmpHost)
	if _, err := r.runPodman(ctx, "cp", name+":"+path, tmpHost); err != nil {
		return nil, err
	}
	return os.ReadFile(tmpHost)
}

// netName returns the network name for a service. When the request specifies an
// explicit Network, services sharing it resolve each other by name; otherwise a
// per-service (same-name) network is used.
func (r *PodmanServiceRunner) netName(name string) string { return name }

// runroot returns the podman runroot actually in use (from storage.conf that
// the container is wired to). The aardvark-dns config lives under
// <runroot>/networks/aardvark-dns, so we must purge there — a hardcoded work
// dir may not match the real runroot.
func (r *PodmanServiceRunner) runroot() string {
	out, err := r.runPodman(context.Background(), "info", "--format", "{{.Store.RunRoot}}")
	if err == nil && strings.TrimSpace(out) != "" {
		return strings.TrimSpace(out)
	}
	return filepath.Join(r.workRoot, "runroot")
}

// ensureNetwork creates a bridge network (with DNS) for the given name. It
// first purges the aardvark-dns config so the DNS server doesn't try to bind
// gateways of stale/removed networks (which no longer exist in this container's
// netns and caused aardvark to abort), then forces-removes any stale network of
// the same name before creating a fresh one.
func (r *PodmanServiceRunner) ensureNetwork(ctx context.Context, name string) error {
	_, _ = r.runPodman(ctx, "network", "rm", "-f", name)
	// Wipe aardvark's per-network DNS config under the real runroot so it only
	// registers the network we are about to create.
	root := r.runroot()
	_ = os.RemoveAll(filepath.Join(root, "networks", "aardvark-dns"))
	_, err := r.runPodman(ctx, "network", "create", "--driver", "bridge", name)
	return err
}

func (r *PodmanServiceRunner) publishArgs(ports map[int]int) []string {
	var args []string
	for c, h := range ports {
		if h == 0 {
			args = append(args, "-p", fmt.Sprintf("127.0.0.1::%d", c))
		} else {
			args = append(args, "-p", fmt.Sprintf("127.0.0.1:%d:%d", h, c))
		}
	}
	return args
}

// qualify prepends the internal registry host to a bare image name.
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

// containerIP returns the primary container's IP on its network.
func (r *PodmanServiceRunner) containerIP(ctx context.Context, name string) (string, error) {
	out, err := r.runPodman(ctx, "inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Launch creates the network, pulls the image, and runs the requested number of
// containers with real cgroup resource limits. When ServiceRequest.Network is
// set, the container joins that shared network (so services in the same network
// resolve each other by name); otherwise a per-service network is created.
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
	ports := req.Ports
	if len(ports) == 0 {
		ports = map[int]int{8080: 0}
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
		args := []string{"run", "-d", "--name", containerName, "--network", net}
		args = append(args, r.publishArgs(ports)...)
		args = append(args, "--restart", restart, "--tls-verify=false", "--pull", "missing")
		for k, v := range req.Labels {
			args = append(args, "--label", k+"="+v)
		}
		for _, e := range req.Env {
			args = append(args, "-e", e)
		}
		if req.CPUs != "" {
			args = append(args, "--cpus", req.CPUs)
		}
		if req.MemoryBytes > 0 {
			args = append(args, "--memory", fmt.Sprintf("%d", req.MemoryBytes))
		}
		args = append(args, r.qualify(req.Image))
		if req.Command != "" {
			args = append(args, "sh", "-c", req.Command)
		}
		if log != nil {
			log("launch " + containerName)
		}
		out, err := r.runPodman(ctx, args...)
		if err != nil {
			return ServiceStatus{}, fmt.Errorf("launch %s: %w", containerName, err)
		}
		ids = append(ids, strings.TrimSpace(out))
		if firstIP == "" {
			firstIP, _ = r.containerIP(ctx, containerName)
		}
	}
	primary := req.Name
	if reps > 1 {
		primary = req.Name + "-0"
	}
	return r.statusOf(ctx, primary, req.Name, reps, ids, firstIP, ports, net), nil
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

// Status inspects a single container by name.
func (r *PodmanServiceRunner) Status(ctx context.Context, name string) (ServiceStatus, error) {
	out, err := r.runPodman(ctx, "inspect", "--format", "{{.State.Status}}", name)
	if err != nil {
		if strings.Contains(err.Error(), "no such") || strings.Contains(err.Error(), "not found") {
			return ServiceStatus{}, os.ErrNotExist
		}
		return ServiceStatus{}, err
	}
	state := strings.TrimSpace(out)
	ip, _ := r.containerIP(ctx, name)
	st := ServiceStatus{Name: name, Kind: "deployment", Phase: state, Replicas: 1, Ready: 1, PodIP: ip, ServiceURL: name}
	if state == "exited" || state == "dead" {
		st.Ready = 0
	}
	return st, nil
}

// Delete removes a service (containers + network). When Network is shared, we
// only remove the per-service network (the same-name one) to avoid tearing
// down a shared group network. Containers are removed by the service name
// prefix.
func (r *PodmanServiceRunner) Delete(ctx context.Context, name string) error {
	candidates := []string{name}
	for i := 0; i < 64; i++ {
		candidates = append(candidates, fmt.Sprintf("%s-%d", name, i))
	}
	for _, c := range candidates {
		_, _ = r.runPodman(ctx, "rm", "-f", c)
	}
	_, _ = r.runPodman(ctx, "network", "rm", "-f", r.netName(name))
	return nil
}

// Scale adjusts replicas by starting/stopping <name>-<N> containers.
func (r *PodmanServiceRunner) Scale(ctx context.Context, name string, replicas int) (ServiceStatus, error) {
	current := 1
	for i := 1; ; i++ {
		c := fmt.Sprintf("%s-%d", name, i)
		if _, err := r.runPodman(ctx, "inspect", c); err == nil {
			current = i + 1
		} else {
			break
		}
	}
	for i := current; i < replicas; i++ {
		_, _ = r.runPodman(ctx, "start", fmt.Sprintf("%s-%d", name, i))
	}
	for i := replicas; i < current; i++ {
		_, _ = r.runPodman(ctx, "stop", fmt.Sprintf("%s-%d", name, i))
	}
	ip, _ := r.containerIP(ctx, name)
	return ServiceStatus{Name: name, Kind: "deployment", Replicas: replicas, Ready: replicas, PodIP: ip, ServiceURL: name}, nil
}

// List lists containers, optionally filtered by network.
func (r *PodmanServiceRunner) List(ctx context.Context, network string) ([]ServiceStatus, error) {
	var extra []string
	if network != "" {
		extra = append(extra, "--filter", "network="+network)
	}
	out, err := r.runPodman(ctx, append([]string{"ps", "-a", "--format", "{{.Names}}"}, extra...)...)
	if err != nil {
		return nil, err
	}
	var list []ServiceStatus
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		st := ServiceStatus{Name: line, Kind: "deployment", Replicas: 1}
		if ip, e := r.containerIP(ctx, line); e == nil {
			st.PodIP = ip
		}
		list = append(list, st)
	}
	return list, nil
}

var _ ServiceRunner = (*PodmanServiceRunner)(nil)
var _ = json.Marshal
var _ = time.Second

// SyncTar extracts a tarball into a running service container at dest. The
// tarball is staged through the work root (podman cp needs a real file). The
// destination directory is created first (podman cp cannot mkdir).
func (r *PodmanServiceRunner) SyncTar(ctx context.Context, name, dest string, tarball []byte) error {
	if dest == "" {
		dest = "/workspace"
	}
	if _, err := r.Exec(ctx, name, "mkdir -p "+shQuoted(dest)); err != nil {
		return fmt.Errorf("sync mkdir: %w", err)
	}
	tmp := filepath.Join(r.workRoot, fmt.Sprintf("sync-%d.tar", time.Now().UnixNano()))
	if err := os.WriteFile(tmp, tarball, 0o644); err != nil {
		return err
	}
	defer os.Remove(tmp)
	_, err := r.runPodman(ctx, "cp", tmp, name+":"+dest)
	return err
}

// shQuoted quotes a path for the plain sh -c used by Exec.
func shQuoted(p string) string {
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}
