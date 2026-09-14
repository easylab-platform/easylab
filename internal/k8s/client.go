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
	"strings"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Client wraps the typed clientset plus easylab's runtime configuration.
type Client struct {
	cs        kubernetes.Interface
	namespace string

	// buildkitImage is the easylab buildkit-worker image (buildkitd +
	// easyworker in one container) run as an ephemeral job for builds.
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
	// runtimes are the sandbox execution profiles (linux/windows/macos/...).
	runtimes map[string]RuntimeProfile

	// egress is the resolved easyproxy sidecar policy.
	egress egressPolicy
}

// egressPolicy carries the resolved easyproxy defaults for this client.
type egressPolicy struct {
	enabled bool
	image   string
	gateway string
	caCert  string
	caKey   string
}

// Config configures a Client.
type Config struct {
	Namespace     string
	BuildkitImage string
	RegistryHost  string
	RegistryToken string
	Proxy         string

	// Egress policy (easyproxy sidecar): ENABLED by default. Every
	// sandbox/job/service carries the easyproxy sidecar with the built-in
	// package-manager rewrite rules. EgressPolicyDisabled=true turns it off
	// globally; a workload's ProxySpec=nil+NoProxy opts out per workload.
	EgressPolicyDisabled bool
	EgressPolicyImage    string // default easylab/easyproxy:v0.1.0
	EgressPolicyGateway  string // rewrite target; default = RegistryHost
	// EgressPolicyCACert/CAKey: the MITM CA PEMs. Generated per namespace by
	// ensureEgressCA on first use when empty; workload pods trust the cert
	// (SSL_CERT_FILE / NODE_EXTRA_CA_CERTS) and easyproxy signs with the key.
	EgressPolicyCACert string
	EgressPolicyCAKey  string
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
		cfg.BuildkitImage = "easylab/buildkit-worker:latest"
	}
	if cfg.RegistryHost == "" {
		cfg.RegistryHost = "easylab"
	}
	gateway := cfg.EgressPolicyGateway
	if gateway == "" {
		gateway = cfg.RegistryHost
	}
	image := cfg.EgressPolicyImage
	if image == "" {
		image = "easylab/easyproxy:v0.1.0"
	}
	// EgressPolicy zero-value (false) must mean ENABLED: the Config is built
	// without setting the field in most call sites. Callers that want the
	// sidecar OFF set EgressPolicyDisabled.
	enabled := !cfg.EgressPolicyDisabled
	return &Client{
		cs: cs, namespace: cfg.Namespace,
		buildkitImage: cfg.BuildkitImage,
		registryHost:  cfg.RegistryHost,
		registryToken: cfg.RegistryToken,
		proxy:         cfg.Proxy,
		runtimes:      loadRuntimes(),
		egress: egressPolicy{
			enabled: enabled,
			image:   image,
			gateway: gateway,
			caCert:  cfg.EgressPolicyCACert,
			caKey:   cfg.EgressPolicyCAKey,
		},
	}
}

// EgressPolicySpec returns the default egress-policy spec for workloads
// (nil when the policy is disabled). Workload specs that set their own
// Proxy override this.
func (c *Client) EgressPolicySpec() *ProxySpec {
	if !c.egress.enabled {
		return nil
	}
	return &ProxySpec{
		Rules:        DefaultRulesYAML(c.egress.gateway),
		Image:        c.egress.image,
		CACertPEM:    c.egress.caCert,
		CAKeyPEM:     c.egress.caKey,
		ClusterCIDRs: c.egressClusterCIDRs(),
	}
}

// egressClusterCIDRs returns the bypass ranges. Overridable via
// EASYLAB_EGRESS_BYPASS_CIDRS; the defaults cover private ranges (cluster
// service/pod networks in typical deployments).
func (c *Client) egressClusterCIDRs() []string {
	if v := os.Getenv("EASYLAB_EGRESS_BYPASS_CIDRS"); v != "" {
		var out []string
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"}
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

// WorkerHostDir returns the node hostPath directory holding the published
// worker binary (empty when the host data dir is unknown).
func (c *Client) WorkerHostDir() string { return workerHostDir() }

// defaultUpstreams mirrors the artifactkit ecosystem table: domains whose
// traffic is steered into the easylab gateway by the default egress policy.
// Keep in sync with easyproxy's defaultrules.go and cmd/easylab/registry.go.
var defaultUpstreams = []egressDomain{
	{Match: []string{"registry-1.docker.io", "docker.io", "production.cloudflare.docker.com"}},
	{Match: []string{"registry.npmjs.org", "*.npmjs.org"}},
	{Match: []string{"pypi.org", "files.pythonhosted.org"}},
	{Match: []string{"proxy.golang.org", "sum.golang.org"}},
	{Match: []string{"crates.io", "index.crates.io", "static.crates.io"}},
	{Match: []string{"repo.maven.apache.org"}},
	{Match: []string{"api.nuget.org", "azuresearch-usnc.nuget.org"}},
	{Match: []string{"rubygems.org", "index.rubygems.org"}},
	{Match: []string{"repo.packagist.org"}},
	{Match: []string{"repo.hex.pm"}},
	{Match: []string{"pub.dev"}},
	{Match: []string{"charts.helm.sh"}},
	{Match: []string{"center.conan.io", "center2.conan.io"}},
	{Match: []string{"api.spm.swift.org"}},
	{Match: []string{"dl-cdn.alpinelinux.org"}},
	{Match: []string{"deb.debian.org", "security.debian.org"}},
	{Match: []string{"archive.ubuntu.com", "security.ubuntu.com"}},
	{Match: []string{"*.elrepo.org", "mirror.stream.centos.org", "dl.fedoraproject.org"}},
	{Match: []string{"huggingface.co", "*.huggingface.co", "cdn-lfs.huggingface.co"}},
	{Match: []string{"repo.anaconda.com", "conda.anaconda.org"}},
	{Match: []string{"cache.nixos.org"}},
}

type egressDomain struct {
	Match []string
}

// DefaultRulesYAML renders the built-in egress policy: package-manager
// upstreams rewritten to the gateway (routing by preserved Host), default
// direct, MITM only for rewrite rules.
func DefaultRulesYAML(gatewayHostPort string) string {
	var b strings.Builder
	b.WriteString("# easyproxy default egress policy (managed by easylab).\n")
	b.WriteString("rules:\n")
	for _, u := range defaultUpstreams {
		quoted := make([]string, 0, len(u.Match))
		for _, m := range u.Match {
			quoted = append(quoted, fmt.Sprintf("%q", m))
		}
		b.WriteString("  - match: [" + strings.Join(quoted, ", ") + "]\n")
		b.WriteString("    action: rewrite\n")
		b.WriteString("    target: \"" + gatewayHostPort + "\"\n")
	}
	b.WriteString("default: direct\n")
	b.WriteString("mitm_default: false\n")
	return b.String()
}
