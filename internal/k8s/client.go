// Package k8s is easylab's Kubernetes execution backend: sandboxes, CI job
// pods, service deployments, and image builds all run as workloads in a target
// namespace, replacing the former in-pod privileged podman sidecar.
//
// Design:
//   - No cluster-scoped permissions: easylab only touches its pre-provisioned
//     namespace.
//   - Builds run in a short-lived, non-privileged rootless buildkit pod.
//   - Workers are reached via in-cluster Service DNS (<name>.<ns>.svc:48080).
package k8s

import (
	"fmt"
	"os"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client wraps the typed clientset plus easylab's runtime configuration.
type Client struct {
	cs        kubernetes.Interface
	namespace string

	// buildkitImage is the official rootless buildkit image used for ephemeral
	// build pods.
	buildkitImage string
	// registryHost is the registry host builds push to and workers pull from.
	// Defaults to the in-cluster Service DNS (easylab.<ns>.svc.cluster.local:80)
	// and can be overridden with EASYLAB_REGISTRY_HOST (e.g. an external
	// ingress). Using Service DNS keeps the deployment portable across
	// clusters/hosts (no ingress IP/domain hardcoded).
	registryHost string
	// registryToken authenticates pushes to the easylab OCI registry.
	registryToken string
	// proxy is the upstream HTTP(S) proxy used by ephemeral build pods to
	// reach public registries (base images).
	proxy string
	// workerImage is the default linux worker image (base+worker) when a job
	// has no explicit container.
	workerImage string
	// runtimes are the sandbox execution profiles (linux/windows/macos/...).
	runtimes map[string]RuntimeProfile
}

// Config configures a Client.
type Config struct {
	Namespace     string
	BuildkitImage string
	RegistryHost  string
	RegistryToken string
	Proxy         string
	WorkerImage   string
}

// New builds a client from the in-cluster config (production) or, when
// KUBECONFIG is set and no in-cluster token exists, from the kubeconfig (dev).
func New(cfg Config) (*Client, error) {
	rc, err := rest.InClusterConfig()
	if err != nil {
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			return nil, fmt.Errorf("k8s: no in-cluster config and KUBECONFIG unset: %w", err)
		}
		rc, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("k8s: kubeconfig: %w", err)
		}
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return nil, fmt.Errorf("k8s: clientset: %w", err)
	}
	return newClient(cs, cfg), nil
}

// NewWithClientset is the test/DI seam.
func NewWithClientset(cs kubernetes.Interface, cfg Config) *Client { return newClient(cs, cfg) }

func newClient(cs kubernetes.Interface, cfg Config) *Client {
	if cfg.Namespace == "" {
		cfg.Namespace = "default"
	}
	if cfg.BuildkitImage == "" {
		cfg.BuildkitImage = "moby/buildkit:rootless"
	}
	if cfg.RegistryHost == "" {
		cfg.RegistryHost = "easylab"
	}
	return &Client{
		cs: cs, namespace: cfg.Namespace,
		buildkitImage: cfg.BuildkitImage,
		registryHost:  cfg.RegistryHost,
		registryToken: cfg.RegistryToken,
		proxy:         cfg.Proxy,
		workerImage:   cfg.WorkerImage,
		runtimes:      loadRuntimes(),
	}
}

// Namespace returns the namespace easylab operates in.
func (c *Client) Namespace() string { return c.namespace }

// RegistryHost returns the in-cluster registry host (host[:port]).
func (c *Client) RegistryHost() string { return c.registryHost }

// RewriteImageRef rewrites an external image ref to a pull-through ref on this
// client's registry host (see the package-level RewriteImageRef).
func (c *Client) RewriteImageRef(ref string) string {
	return RewriteImageRef(ref, c.registryHost)
}

// Clientset exposes the typed clientset.
func (c *Client) Clientset() kubernetes.Interface { return c.cs }

// ServiceDNS returns the FQDN for a workload's Service.
func (c *Client) ServiceDNS(name string) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", name, c.namespace)
}
