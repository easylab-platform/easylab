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

	// egress is the resolved easysidecar policy.
	egress egressPolicy
}

// egressPolicy carries the resolved easysidecar defaults for this client.
type egressPolicy struct {
	enabled        bool
	image          string
	gateway        string
	caCert         string
	caKey          string
	spoofDNS       string
	upstreamProxy  string
	clusterDomain  string
	capture        bool
	captureForward bool
	mitmDefault    bool
	udpAllow       []string
	udpMode        string
	defaultMode    string
	exemptCIDRs    []string
	webPorts       []int
}

// Config configures a Client.
type Config struct {
	Namespace     string
	BuildkitImage string
	RegistryHost  string
	RegistryToken string
	Proxy         string

	// Egress policy (easysidecar): ENABLED by default. Every
	// sandbox/job/service carries the easysidecar sidecar with the built-in
	// package-manager rewrite rules. EgressPolicyDisabled=true turns it off
	// globally; a workload's ProxySpec=nil+NoProxy opts out per workload.
	EgressPolicyDisabled bool
	EgressPolicyImage    string // default easylab/easysidecar:v0.1.0
	EgressPolicyGateway  string // rewrite target; default = RegistryHost
	// EgressPolicyCACert/CAKey: the MITM CA PEMs. Generated per namespace by
	// ensureEgressCA on first use when empty; workload pods trust the cert
	// (SSL_CERT_FILE / NODE_EXTRA_CA_CERTS) and easysidecar signs with the key.
	EgressPolicyCACert string
	EgressPolicyCAKey  string
	// EgressPolicyMode selects the interception mechanism:
	//   ""/"spoof"  DNS-spoof (default): the sidecar is the Pod's resolver and
	//               its :443/:80 listener; no privileges.
	//   "capture"   privileged all-port interception: an init container
	//               redirects every outbound TCP connection to the sidecar,
	//               which recovers the destination via SO_ORIGINAL_DST. Covers
	//               arbitrary ports and non-DNS-aware clients at the cost of
	//               NET_ADMIN on the sidecar and init container.
	EgressPolicyMode string
	// EgressPolicySpoofDNS is the cluster resolver the spoof sidecar
	// forwards real queries to (CoreDNS service IP).
	EgressPolicySpoofDNS string
	// EgressPolicyUpstreamProxy is an optional egress proxy (mihomo) DIRECT
	// traffic is sent through in spoof mode.
	EgressPolicyUpstreamProxy string
	// ClusterDomain is the cluster DNS domain (default cluster.local).
	ClusterDomain string

	// EgressPolicyMitmDefault decrypts EVERY intercepted TLS connection, not
	// just rewrite rules. It is a strong posture: clients with pinned
	// certificates or their own trust store will fail. Off by default.
	EgressPolicyMitmDefault bool
	// EgressPolicyCaptureUDPAllow lists UDP endpoints that always pass
	// (host[:port] or cidr[:port]) when capture mode is on.
	EgressPolicyCaptureUDPAllow []string
	// EgressPolicyCaptureUDPMode is "log" (default) or "reject" for UDP that is
	// neither DNS, h3 nor allowed.
	EgressPolicyCaptureUDPMode string
	// EgressPolicyCaptureDefaultMode is "log" (default) or "reject" for other
	// egress (ICMP/raw/uncaptured).
	EgressPolicyCaptureDefaultMode string
	// EgressPolicyCaptureExemptCIDRs always pass (resolver/apiserver/node/pod/
	// service CIDRs).
	EgressPolicyCaptureExemptCIDRs []string
	// EgressPolicyCaptureForward also serves forwarded (VM guest) traffic.
	EgressPolicyCaptureForward bool
	// EgressPolicyCaptureTCPPorts are the destination ports proxied as web
	// (HTTP/h2c) in capture mode; empty uses the sidecar default (80,443).
	EgressPolicyCaptureTCPPorts []int
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
		image = "easylab/easysidecar:v0.1.0"
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
			enabled:        enabled,
			image:          image,
			gateway:        gateway,
			caCert:         cfg.EgressPolicyCACert,
			caKey:          cfg.EgressPolicyCAKey,
			spoofDNS:       cfg.EgressPolicySpoofDNS,
			upstreamProxy:  cfg.EgressPolicyUpstreamProxy,
			clusterDomain:  cfg.ClusterDomain,
			capture:        cfg.EgressPolicyMode == "capture",
			captureForward: cfg.EgressPolicyCaptureForward,
			mitmDefault:    cfg.EgressPolicyMitmDefault,
			udpAllow:       cfg.EgressPolicyCaptureUDPAllow,
			udpMode:        cfg.EgressPolicyCaptureUDPMode,
			defaultMode:    cfg.EgressPolicyCaptureDefaultMode,
			exemptCIDRs:    cfg.EgressPolicyCaptureExemptCIDRs,
		},
	}
}

// EgressPolicySpec returns the default egress-policy spec for workloads
// (nil when the policy is disabled). Workload specs that set their own
// Proxy override this.
func (c *Client) EgressPolicySpec() *ProxySpec {
	return c.egressSpecFor(false)
}

// egressSpecFor builds the default spec. isService selects the redirect mode
// for service deployments (they may bind 80/443, so spoof would conflict);
// sandboxes and CI jobs use spoof when no explicit mode is configured.
func (c *Client) egressSpecFor(isService bool) *ProxySpec {
	if !c.egress.enabled {
		return nil
	}
	return &ProxySpec{
		Rules:            DefaultRulesYAML(c.egress.gateway, c.egress.mitmDefault),
		Image:            c.egress.image,
		CACertPEM:        c.egress.caCert,
		CAKeyPEM:         c.egress.caKey,
		SpoofUpstreamDNS: c.egress.spoofDNS,
		UpstreamProxy:    c.egress.upstreamProxy,
		ClusterDomain:    c.egress.clusterDomain,
		Capture:          c.egress.capture,
		CaptureForward:   c.egress.captureForward,
		MitmDefault:      c.egress.mitmDefault,
		UDPAllow:         c.egress.udpAllow,
		UDPMode:          c.egress.udpMode,
		DefaultMode:      c.egress.defaultMode,
		ExemptCIDRs:      c.egress.exemptCIDRs,
		WebPorts:         c.egress.webPorts,
	}
}

// EgressPolicySpecService is the service-workload variant (redirect by
// default so it never competes for :443).
func (c *Client) EgressPolicySpecService() *ProxySpec {
	return c.egressSpecFor(true)
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
// Keep in sync with easysidecar's rule/defaultrules.go and cmd/easylab/registry.go.
var defaultUpstreams = []egressDomain{
	{Match: []string{"registry-1.docker.io", "docker.io", "index.docker.io"}},
	{Match: []string{"ghcr.io", "quay.io", "gcr.io", "registry.k8s.io",
		"mcr.microsoft.com", "public.ecr.aws", "nvcr.io"}},
	// Container blob CDNs stay direct: adapters follow the 307 themselves.
	{Match: []string{"production.cloudflare.docker.com", "*.cloudflarestorage.com"}},
	{Match: []string{"registry.npmjs.org", "*.npmjs.org"}},
	{Match: []string{"npm.jsr.io"}, Add: "/pkgs/npm"},
	{Match: []string{"pypi.org", "files.pythonhosted.org"}},
	{Match: []string{"proxy.golang.org", "sum.golang.org"}},
	{Match: []string{"crates.io", "index.crates.io", "static.crates.io"}},
	{Match: []string{"repo.maven.apache.org"}},
	// Maven-layout mirrors (host-driven maven adapter).
	{Match: []string{"dl.google.com"}, Strip: "/dl/android/maven2", Add: "/pkgs/maven"},
	{Match: []string{"plugins.gradle.org"}, Strip: "/m2", Add: "/pkgs/maven"},
	{Match: []string{"repo.clojars.org"}, Add: "/pkgs/maven"},
	{Match: []string{"repo.spring.io"}, Strip: "/release", Add: "/pkgs/maven"},
	{Match: []string{"jitpack.io"}, Add: "/pkgs/maven"},
	{Match: []string{"api.nuget.org", "azuresearch-usnc.nuget.org"}},
	{Match: []string{"rubygems.org", "index.rubygems.org"}},
	{Match: []string{"repo.packagist.org"}},
	{Match: []string{"repo.hex.pm"}},
	{Match: []string{"hex.pm", "api.hex.pm"}},
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
	// Source mirrors: git smart-HTTP and Ivy repositories.
	{Match: []string{"github.com", "codeload.github.com"}},
	{Match: []string{"repo.scala-sbt.org", "scala.jfrog.io"}},
	// Plain-HTTP package trees (Haskell, R, Perl, Lua) + Julia's package server.
	{Match: []string{"hackage.haskell.org"}},
	{Match: []string{"cran.r-project.org"}},
	{Match: []string{"cpan.metacpan.org"}},
	{Match: []string{"luarocks.org"}},
	{Match: []string{"pkg.julialang.org", "*.pkg.julialang.org"}},
	// Additional plain-HTTP trees.
	{Match: []string{"jsr.io"}, Add: "/pkgs/jsr"},
	{Match: []string{"opam.ocaml.org"}, Add: "/pkgs/opam"},
	{Match: []string{"stackage.org"}, Add: "/pkgs/stackage"},
	{Match: []string{"pecl.php.net"}, Add: "/pkgs/pecl"},
	{Match: []string{"bcr.bazel.build"}, Add: "/pkgs/bazel"},
	{Match: []string{"updates.jenkins.io"}, Add: "/pkgs/jenkins"},
}

type egressDomain struct {
	Match []string
	// Strip is a leading path prefix removed before Add is applied (mirrors
	// whose path shape differs from the adapter's mount).
	Strip string
	// Add is the adapter mount prepended after Strip.
	Add string
}

// DefaultRulesYAML renders the built-in egress policy: package-manager
// upstreams rewritten to the gateway (routing by preserved Host), default
// direct, MITM only for rewrite rules.
func DefaultRulesYAML(gatewayHostPort string, mitmDefault bool) string {
	var b strings.Builder
	b.WriteString("# easysidecar default egress policy (managed by easylab).\n")
	b.WriteString("rules:\n")
	for _, u := range defaultUpstreams {
		quoted := make([]string, 0, len(u.Match))
		for _, m := range u.Match {
			quoted = append(quoted, fmt.Sprintf("%q", m))
		}
		b.WriteString("  - match: [" + strings.Join(quoted, ", ") + "]\n")
		b.WriteString("    action: rewrite\n")
		b.WriteString("    target: \"" + gatewayHostPort + "\"\n")
		if u.Strip != "" {
			b.WriteString("    strip_prefix: \"" + u.Strip + "\"\n")
		}
		if u.Add != "" {
			b.WriteString("    add_prefix: \"" + u.Add + "\"\n")
		}
	}
	b.WriteString("default: direct\n")
	if mitmDefault {
		b.WriteString("mitm_default: true\n")
	} else {
		b.WriteString("mitm_default: false\n")
	}
	return b.String()
}
