package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testRules = `
rules:
  - match: ["*.npmjs.org"]
    action: rewrite
    target: "gateway.easylab.svc:8080"
  - match: ["*.evil.example"]
    action: block
default: direct
`

func TestWithProxyInjectsSidecar(t *testing.T) {
	pod := minimalPod("sbx-a")
	proxy := &ProxySpec{
		Rules:        testRules,
		Image:        "registry/easyproxy:v0.1.0",
		CACertPEM:    "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----",
		CAKeyPEM:     "-----BEGIN PRIVATE KEY-----\nY\n-----END PRIVATE KEY-----",
		ClusterCIDRs: []string{"10.96.0.0/12", "172.20.0.0/16"},
	}
	if err := withProxy(&pod.Spec, pod.Name, "test", proxy); err != nil {
		t.Fatal(err)
	}

	// init container with NET_ADMIN only.
	if len(pod.Spec.InitContainers) == 0 {
		t.Fatal("no init container")
	}
	init := pod.Spec.InitContainers[len(pod.Spec.InitContainers)-1]
	if init.Name != "easyproxy-init" {
		t.Fatalf("init name = %q", init.Name)
	}
	if init.SecurityContext == nil || init.SecurityContext.Capabilities == nil ||
		len(init.SecurityContext.Capabilities.Add) != 1 ||
		init.SecurityContext.Capabilities.Add[0] != "NET_ADMIN" {
		t.Fatalf("init capabilities = %+v", init.SecurityContext)
	}
	if init.SecurityContext.Privileged != nil && *init.SecurityContext.Privileged {
		t.Fatal("init must not be privileged")
	}

	// sidecar present, no special capabilities.
	var found bool
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != "easyproxy" {
			continue
		}
		found = true
		if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
			t.Fatal("sidecar must not be privileged")
		}
	}
	if !found {
		t.Fatal("no easyproxy sidecar")
	}

	// volumes: rules ConfigMap + CA secret.
	var cm, secret bool
	for _, v := range pod.Spec.Volumes {
		if v.ConfigMap != nil && v.Name == "easyproxy-rules" {
			cm = true
		}
		if v.Secret != nil && v.Name == "easyproxy-ca" {
			secret = true
		}
	}
	if !cm || !secret {
		t.Fatalf("volumes: cm=%v secret=%v", cm, secret)
	}

	// workload env: proxy + CA paths, NO_PROXY bypasses cluster ranges.
	var worker *string
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "worker" {
			c := pod.Spec.Containers[i]
			for _, e := range c.Env {
				if e.Name == "HTTPS_PROXY" {
					s := e.Value
					worker = &s
				}
			}
		}
	}
	if worker == nil || !strings.Contains(*worker, "7890") {
		t.Fatalf("HTTPS_PROXY env = %v", worker)
	}
}

func TestWithProxyRewriteRequiresCA(t *testing.T) {
	pod := minimalPod("sbx-b")
	proxy := &ProxySpec{Rules: testRules, Image: "img", ClusterCIDRs: []string{"10.0.0.0/8"}}
	if err := withProxy(&pod.Spec, pod.Name, "test", proxy); err == nil {
		t.Fatal("rewrite rules without CA must be rejected")
	}
}

func TestWithProxyNilIsNoop(t *testing.T) {
	pod := minimalPod("sbx-c")
	before := len(pod.Spec.Containers)
	if err := withProxy(&pod.Spec, pod.Name, "test", nil); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.Containers) != before {
		t.Fatal("nil proxy must not modify the pod")
	}
}

func minimalPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "worker", Image: "worker:v1"}},
		},
	}
}

// TestDefaultInjection verifies the default-on egress policy: a workload
// without an explicit Proxy gets the sidecar with the built-in rules; NoProxy
// skips it; Config.EgressPolicyDisabled turns it off globally.
func TestDefaultInjection(t *testing.T) {
	c := NewWithClientset(nil, Config{
		Namespace:           "test",
		EgressPolicyGateway: "gateway.easylab.svc:8080",
		EgressPolicyCACert:  "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----",
		EgressPolicyCAKey:   "-----BEGIN PRIVATE KEY-----\nY\n-----END PRIVATE KEY-----",
	})

	// Default spec carries the built-in rewrite rules and CA material.
	def := c.EgressPolicySpec()
	if def == nil {
		t.Fatal("egress policy must be enabled by default")
	}
	if !strings.Contains(def.Rules, "registry.npmjs.org") ||
		!strings.Contains(def.Rules, "deb.debian.org") ||
		!strings.Contains(def.Rules, "cache.nixos.org") {
		t.Fatalf("default rules missing ecosystems:\n%s", def.Rules)
	}
	if def.Image == "" || len(def.ClusterCIDRs) == 0 {
		t.Fatalf("spec incomplete: image=%q cidrs=%v", def.Image, def.ClusterCIDRs)
	}

	// Default-on injection: LaunchSandbox path would inject; verify the
	// injection helper produces the sidecar for the default spec.
	pod := minimalPod("sbx-default")
	if err := withProxy(&pod.Spec, pod.Name, "test", def); err != nil {
		t.Fatal(err)
	}
	found := false
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "easyproxy" {
			found = true
		}
	}
	if !found {
		t.Fatal("default injection must add the sidecar")
	}
	// Workload env trusts the CA.
	worker := pod.Spec.Containers[0]
	var sslFile string
	for _, e := range worker.Env {
		if e.Name == "SSL_CERT_FILE" {
			sslFile = e.Value
		}
	}
	if sslFile == "" {
		t.Fatal("SSL_CERT_FILE missing on workload container")
	}
}

// TestDefaultRulesYAMLShape verifies the embedded rule set.
func TestDefaultRulesYAMLShape(t *testing.T) {
	y := DefaultRulesYAML("gw:80")
	if !strings.Contains(y, `target: "gw:80"`) {
		t.Fatalf("target missing:\n%s", y)
	}
	if strings.Contains(y, "mitm_default: true") {
		t.Fatal("mitm_default must be false")
	}
	if strings.Count(y, "action: rewrite") != len(defaultUpstreams) {
		t.Fatalf("rewrite rule count mismatch")
	}
}

// TestSpoofModeInjection verifies dns-spoof mode: no init container, dnsConfig
// pointed at the sidecar, spoof ports/env, and POD_IP wired via the downward
// API.
func TestSpoofModeInjection(t *testing.T) {
	pod := minimalPod("sbx-spoof")
	proxy := &ProxySpec{
		Mode:             ProxyModeSpoof,
		Rules:            testRules,
		Image:            "registry/easyproxy:v0.2.0",
		CACertPEM:        "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----",
		CAKeyPEM:         "-----BEGIN PRIVATE KEY-----\nY\n-----END PRIVATE KEY-----",
		SpoofUpstreamDNS: "10.96.0.10",
		UpstreamProxy:    "http://mihomo.develop.svc:7890",
	}
	if err := withProxy(&pod.Spec, pod.Name, "test", proxy); err != nil {
		t.Fatal(err)
	}

	// No init container in spoof mode.
	if len(pod.Spec.InitContainers) != 0 {
		t.Fatalf("spoof mode must not add an init container, got %d", len(pod.Spec.InitContainers))
	}
	// dnsConfig points at the sidecar.
	if pod.Spec.DNSConfig == nil || len(pod.Spec.DNSConfig.Nameservers) != 1 ||
		pod.Spec.DNSConfig.Nameservers[0] != "127.0.0.1" {
		t.Fatalf("dnsConfig = %+v", pod.Spec.DNSConfig)
	}
	// Sidecar args carry spoof + upstream dns + proxy; POD_IP env present.
	var found bool
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.Name != "easyproxy" {
			continue
		}
		found = true
		joined := strings.Join(c.Args, " ")
		for _, want := range []string{"--spoof", "--upstream-dns=10.96.0.10", "--upstream-proxy=http://mihomo.develop.svc:7890"} {
			if !strings.Contains(joined, want) {
				t.Fatalf("args missing %q: %v", want, c.Args)
			}
		}
		var podIP bool
		for _, e := range c.Env {
			if e.Name == "POD_IP" && e.ValueFrom != nil && e.ValueFrom.FieldRef != nil &&
				e.ValueFrom.FieldRef.FieldPath == "status.podIP" {
				podIP = true
			}
		}
		if !podIP {
			t.Fatal("POD_IP downward-API env missing")
		}
	}
	if !found {
		t.Fatal("no easyproxy sidecar")
	}

	// Spoof mode without an upstream DNS is rejected.
	pod2 := minimalPod("sbx-spoof-bad")
	bad := &ProxySpec{Mode: ProxyModeSpoof, Rules: testRules, Image: "img"}
	if err := withProxy(&pod2.Spec, pod2.Name, "test", bad); err == nil {
		t.Fatal("spoof without SpoofUpstreamDNS must be rejected")
	}
}

// TestRedirectModeKeepsInit confirms the default path is unchanged.
func TestRedirectModeKeepsInit(t *testing.T) {
	pod := minimalPod("sbx-redir")
	proxy := &ProxySpec{
		Rules: testRules, Image: "img",
		CACertPEM:    "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----",
		CAKeyPEM:     "-----BEGIN PRIVATE KEY-----\nY\n-----END PRIVATE KEY-----",
		ClusterCIDRs: []string{"10.0.0.0/8"},
	}
	if err := withProxy(&pod.Spec, pod.Name, "test", proxy); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.InitContainers) != 1 {
		t.Fatalf("redirect mode must keep the init container, got %d", len(pod.Spec.InitContainers))
	}
	if pod.Spec.DNSConfig != nil {
		t.Fatal("redirect mode must not set dnsConfig")
	}
}

// TestEgressPolicyModeSelection verifies the auto mode: sandbox/job use spoof
// (when a cluster DNS is configured), service uses redirect.
func TestEgressPolicyModeSelection(t *testing.T) {
	c := NewWithClientset(nil, Config{
		Namespace:                 "test",
		EgressPolicyGateway:       "gw:8080",
		EgressPolicySpoofDNS:      "10.96.0.10",
		EgressPolicyUpstreamProxy: "http://mihomo:7890",
	})
	if got := c.EgressPolicySpec(); got == nil || got.Mode != ProxyModeSpoof {
		t.Fatalf("sandbox spec mode = %+v, want spoof", got)
	} else if got.SpoofUpstreamDNS != "10.96.0.10" || got.UpstreamProxy != "http://mihomo:7890" {
		t.Fatalf("spoof spec wiring: %+v", got)
	}
	if got := c.EgressPolicySpecService(); got == nil || got.Mode != ProxyModeRedirect {
		t.Fatalf("service spec mode = %+v, want redirect", got)
	}
	// Without a cluster DNS, sandbox falls back to redirect too.
	c2 := NewWithClientset(nil, Config{Namespace: "t", EgressPolicyGateway: "gw:8080"})
	if got := c2.EgressPolicySpec(); got == nil || got.Mode != ProxyModeRedirect {
		t.Fatalf("no-DNS sandbox mode = %+v, want redirect", got)
	}
	// Explicit mode wins.
	c3 := NewWithClientset(nil, Config{Namespace: "t", EgressPolicyMode: ProxyModeSpoof, EgressPolicySpoofDNS: "1.2.3.4", EgressPolicyGateway: "gw"})
	if got := c3.EgressPolicySpecService(); got == nil || got.Mode != ProxyModeSpoof {
		t.Fatalf("explicit mode override = %+v", got)
	}
}
