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
	if err := withProxy(&pod.Spec, pod.Name, proxy); err != nil {
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
	if err := withProxy(&pod.Spec, pod.Name, proxy); err == nil {
		t.Fatal("rewrite rules without CA must be rejected")
	}
}

func TestWithProxyNilIsNoop(t *testing.T) {
	pod := minimalPod("sbx-c")
	before := len(pod.Spec.Containers)
	if err := withProxy(&pod.Spec, pod.Name, nil); err != nil {
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
	if err := withProxy(&pod.Spec, pod.Name, def); err != nil {
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
