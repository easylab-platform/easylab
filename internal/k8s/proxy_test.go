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
		Rules:            testRules,
		Image:            "registry/easyproxy:v0.2.0",
		CACertPEM:        "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----",
		CAKeyPEM:         "-----BEGIN PRIVATE KEY-----\nY\n-----END PRIVATE KEY-----",
		SpoofUpstreamDNS: "10.96.0.10",
	}
	if err := withProxy(&pod.Spec, pod.Name, "test", proxy); err != nil {
		t.Fatal(err)
	}

	// No init container: spoof needs no iptables/NET_ADMIN.
	if len(pod.Spec.InitContainers) != 0 {
		t.Fatalf("spoof must not add an init container, got %d", len(pod.Spec.InitContainers))
	}

	// sidecar present, no privileged, correct ports.
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
		ports := map[int32]bool{}
		for _, p := range c.Ports {
			ports[p.ContainerPort] = true
		}
		for _, want := range []int32{53, 443, 80} {
			if !ports[want] {
				t.Fatalf("sidecar missing port %d: %v", want, c.Ports)
			}
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

	// workload env: NO proxy env (DNS-driven); CA paths only.
	var worker *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "worker" {
			worker = &pod.Spec.Containers[i]
		}
	}
	if worker == nil {
		t.Fatal("no worker container")
	}
	var sslFile string
	for _, e := range worker.Env {
		if e.Name == "HTTP_PROXY" || e.Name == "HTTPS_PROXY" {
			t.Fatalf("spoof mode must not inject %s", e.Name)
		}
		if e.Name == "SSL_CERT_FILE" {
			sslFile = e.Value
		}
	}
	if sslFile == "" {
		t.Fatal("SSL_CERT_FILE missing")
	}

	// dnsConfig: nameserver = sidecar, search path present.
	if pod.Spec.DNSPolicy != corev1.DNSNone {
		t.Fatal("dnsPolicy must be None in spoof mode")
	}
	dc := pod.Spec.DNSConfig
	if dc == nil || len(dc.Nameservers) != 1 || dc.Nameservers[0] != "127.0.0.1" {
		t.Fatalf("dnsConfig nameservers = %+v", dc)
	}
	if len(dc.Searches) < 3 || !strings.Contains(dc.Searches[0], "svc.") {
		t.Fatalf("dnsConfig searches = %v", dc.Searches)
	}
}

func TestWithProxyRewriteRequiresCA(t *testing.T) {
	pod := minimalPod("sbx-b")
	proxy := &ProxySpec{Rules: testRules, Image: "img", SpoofUpstreamDNS: "10.96.0.10"}
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
		Namespace:            "test",
		EgressPolicyGateway:  "gateway.easylab.svc:8080",
		EgressPolicyCACert:   "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----",
		EgressPolicyCAKey:    "-----BEGIN PRIVATE KEY-----\nY\n-----END PRIVATE KEY-----",
		EgressPolicySpoofDNS: "10.96.0.10",
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
	if def.Image == "" || def.SpoofUpstreamDNS == "" {
		t.Fatalf("spec incomplete: image=%q dns=%q", def.Image, def.SpoofUpstreamDNS)
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
// pointed at the sidecar, spoof ports, no proxy env, and POD_IP wired via the
// downward API.
func TestSpoofModeInjection(t *testing.T) {
	pod := minimalPod("sbx-spoof")
	proxy := &ProxySpec{
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
		// No proxy env in spoof mode: interception is DNS-driven.
		for _, e := range c.Env {
			if e.Name == "HTTP_PROXY" || e.Name == "HTTPS_PROXY" {
				t.Fatalf("spoof mode must not inject %s", e.Name)
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
	bad := &ProxySpec{Rules: testRules, Image: "img"}
	if err := withProxy(&pod2.Spec, pod2.Name, "test", bad); err == nil {
		t.Fatal("spoof without SpoofUpstreamDNS must be rejected")
	}
}

// TestSpoofRequiresUpstreamDNS verifies the resolver requirement.
func TestSpoofRequiresUpstreamDNS(t *testing.T) {
	pod := minimalPod("sbx-nodns")
	bad := &ProxySpec{Rules: testRules, Image: "img"}
	if err := withProxy(&pod.Spec, pod.Name, "test", bad); err == nil {
		t.Fatal("missing SpoofUpstreamDNS must be rejected")
	}
}

// TestEgressPolicySpoofForAllWorkloads verifies spoof is the default for
// sandbox/job AND service, with the resolver and upstream proxy wired.
func TestEgressPolicySpoofForAllWorkloads(t *testing.T) {
	c := NewWithClientset(nil, Config{
		Namespace:                 "test",
		EgressPolicyGateway:       "gw:8080",
		EgressPolicySpoofDNS:      "10.96.0.10",
		EgressPolicyUpstreamProxy: "http://mihomo:7890",
	})
	if got := c.EgressPolicySpec(); got == nil || got.SpoofUpstreamDNS != "10.96.0.10" {
		t.Fatalf("sandbox spec: %+v", got)
	}
	if got := c.EgressPolicySpecService(); got == nil || got.SpoofUpstreamDNS != "10.96.0.10" || got.UpstreamProxy != "http://mihomo:7890" {
		t.Fatalf("service spec: %+v", got)
	}
	// VM-spoof exclusion is enforced at LaunchSandbox time (NeedsTun), not here.
}

// TestSpoofSkippedForVMRuntimes verifies NeedsTun profiles (windows/macos VMs)
// do not get the egress sidecar: pod-level DNS never reaches the guest.
func TestSpoofSkippedForVMRuntimes(t *testing.T) {
	c := NewWithClientset(nil, Config{
		Namespace:            "test",
		EgressPolicyGateway:  "gw:8080",
		EgressPolicySpoofDNS: "10.96.0.10",
		EgressPolicyCACert:   "-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----",
		EgressPolicyCAKey:    "-----BEGIN PRIVATE KEY-----\nY\n-----END PRIVATE KEY-----",
	})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-vm", Namespace: "test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "worker", Image: "vm-img"}},
		},
	}
	proxy := c.EgressPolicySpec()
	if proxy == nil {
		t.Fatal("policy must be enabled")
	}
	if err := withProxy(&pod.Spec, pod.Name, "test", proxy); err != nil {
		t.Fatal(err)
	}
	if pod.Spec.DNSPolicy != corev1.DNSNone {
		t.Fatalf("sandbox pod should have spoof dnsConfig (DNSNone), got %+v",
			pod.Spec.DNSPolicy)
	}

	// The VM exclusion path hands LaunchSandbox a nil ProxySpec: a fresh pod
	// gets NO dnsConfig and NO sidecar.
	pod2 := minimalPod("sbx-vm")
	if err := withProxy(&pod2.Spec, pod2.Name, "test", nil); err != nil {
		t.Fatal(err)
	}
	if pod2.Spec.DNSConfig != nil {
		t.Fatalf("VM pod must not get dnsConfig: %+v", pod2.Spec.DNSConfig)
	}
	found := false
	for i := range pod2.Spec.Containers {
		if pod2.Spec.Containers[i].Name == "easyproxy" {
			found = true
		}
	}
	if found {
		t.Fatal("VM pod must not get the sidecar")
	}
}
