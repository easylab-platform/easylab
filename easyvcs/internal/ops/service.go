package ops

import (
	"context"
	"os"
	"time"
)

// envOr returns the environment variable value or a default.
func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ServiceRequest declares a service to launch as a podman container running
// INSIDE the EasyLab container (the fully self-contained "internal podman"
// backend). Every service gets its own podman network (bridge + embedded DNS);
// services sharing a Group/Network resolve each other by name
// (`http://<svc>:<port>`), and ports are published on the EasyLab loopback so a
// reverse proxy can reach them. Real cgroup resource limits are applied via
// --cpus/--memory; the pod must mount /sys/fs/cgroup read-write (privileged).
type ServiceRequest struct {
	// Name is the container/network name (must be unique), e.g. "api".
	Name string `json:"name"`
	// Image is a container image reference (internal /v2 or a hub mirror),
	// e.g. "127.0.0.1:8080/team/app/api:ok".
	Image string `json:"image"`
	// Command, if set, overrides the image entrypoint (sh -c). Empty = image cmd.
	Command string `json:"command,omitempty"`
	// Ports maps container ports to published host ports; keys are container
	// ports, values the host port bound on the host loopback (0 = auto-assign).
	// An empty map publishes no ports.
	Ports map[int]int `json:"ports,omitempty"`
	// Env is a set of KEY=VAL for the container.
	Env []string `json:"env,omitempty"`
	// Replicas is the number of identical containers to run (each a distinct
	// name: <name>-<n>). 0/1 => a single container named <name>.
	Replicas int `json:"replicas,omitempty"`
	// Restart policy: "no"|"always"|"on-failure"|"unless-stopped".
	Restart string `json:"restart,omitempty"`
	// CPUs (e.g. "0.5") and MemoryBytes bound the container via cgroups.
	CPUs        string `json:"cpus,omitempty"`
	MemoryBytes uint64 `json:"memory_bytes,omitempty"`
	// Network is the podman network name. If empty, a network is derived from
	// Group (else Name) and shared by all replicas. Services on the same network
	// resolve each other by container name.
	Network string `json:"network,omitempty"`
	// Group groups related services onto the same network so they can address
	// each other by `<name>` (application-internal DNS). When Network is empty,
	// the group name is used as the network name; when Group is empty too, the
	// per-service Name is used.
	Group string `json:"group,omitempty"`
	// Labels added to the container.
	Labels map[string]string `json:"labels,omitempty"`
	// Timeout bounds the launch (image pull may be large); 0 = default.
	Timeout time.Duration `json:"-"`
}

// NetworkName returns the effective network name for a service: explicit
// Network wins; otherwise Group (shared by the app) else Name (per-service).
func (r ServiceRequest) NetworkName() string {
	if r.Network != "" {
		return r.Network
	}
	if r.Group != "" {
		return r.Group
	}
	return r.Name
}

// ServiceStatus describes a running service.
type ServiceStatus struct {
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	Replicas     int      `json:"replicas"`
	Ready        int      `json:"ready"`
	Phase        string   `json:"phase"`
	PodIP        string   `json:"pod_ip"`
	ContainerIDs []string `json:"container_ids,omitempty"`
	// WorkerURL is the first published loopback URL. Empty if no ports were
	// published.
	WorkerURL string `json:"worker_url,omitempty"`
	// PublicURL is reserved for future external-domain mapping (empty now).
	PublicURL string `json:"public_url,omitempty"`
	// ServiceURL is the in-network DNS name others use to reach this service
	// (bare <name> resolves within its podman network).
	ServiceURL string `json:"service_url,omitempty"`
	// Network is the podman network the service is attached to.
	Network string `json:"network,omitempty"`
}

// ServiceRunner is the service launch seam. The only backend is the internal
// podman runner (fully self-contained) — see service_podman.go.
type ServiceRunner interface {
	Launch(ctx context.Context, req ServiceRequest, log func(string)) (ServiceStatus, error)
	Status(ctx context.Context, name string) (ServiceStatus, error)
	Delete(ctx context.Context, name string) error
	Scale(ctx context.Context, name string, replicas int) (ServiceStatus, error)
	List(ctx context.Context, network string) ([]ServiceStatus, error)
	Name() string
}

// firstHostPort returns the lowest nonzero host port from the ports map.
func firstHostPort(ports map[int]int) int {
	best := 0
	bestHost := 0
	for c, h := range ports {
		if h != 0 && (best == 0 || c < best) {
			best = c
			bestHost = h
		}
	}
	return bestHost
}
