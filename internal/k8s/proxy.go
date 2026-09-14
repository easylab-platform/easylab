package k8s

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProxySpec describes an optional egress-policy sidecar for a workload:
// easyproxy intercepts the Pod's outbound 80/443 (plus DNS) and applies the
// block/direct/rewrite rule set. Nil on the workload spec means no proxy —
// the Pod is generated exactly as before.
type ProxySpec struct {
	// Rules is the YAML rule set text (see easyproxy README for the schema).
	Rules string
	// MitmDefault decrypts every intercepted TLS connection, not just
	// rewrite rules. Off by default.
	MitmDefault bool
	// InterceptAllTCP extends interception from 80/443 to all TCP
	// (cluster bypass still applies). Off by default.
	InterceptAllTCP bool
	// CACertPEM is the MITM CA certificate distributed to the workload
	// (SSL_CERT_FILE / NODE_EXTRA_CA_CERTS). Required when any rewrite rule
	// is present.
	CACertPEM string
	// CAKeyPEM is the MITM CA private key (easyproxy signs leaf certificates
	// with it). Namespace-scoped secret material.
	CAKeyPEM string
	// Image is the easyproxy image (single image, two roles). Empty uses the
	// chart default wired by the caller.
	Image string
	// ClusterCIDRs are the RETURN (bypass) ranges — service/pod CIDRs, the
	// internal registry, NATS. Required.
	ClusterCIDRs []string
}

// proxyConstants are the well-known ports easyproxy listens on inside the Pod.
const (
	proxyRedirPort  = int32(7893)
	proxyDNSPort    = int32(7894)
	proxyConnectStr = "127.0.0.1:7890"
	proxyCAPath     = "/etc/easyproxy/ca.crt"
)

// withProxy injects the easyproxy init container, sidecar, volumes, and env
// into a Pod spec. It appends to the existing containers; call it BEFORE the
// pod is created. proxy == nil is a no-op.
func withProxy(pod *corev1.PodSpec, workloadName string, proxy *ProxySpec) error {
	if proxy == nil {
		return nil
	}
	if proxy.Image == "" {
		return fmt.Errorf("proxy: image required")
	}
	if strings.TrimSpace(proxy.Rules) == "" {
		return fmt.Errorf("proxy: rules required")
	}
	if len(proxy.ClusterCIDRs) == 0 {
		return fmt.Errorf("proxy: cluster CIDRs required (bypass ranges)")
	}
	rewriteRules := strings.Contains(proxy.Rules, "action: rewrite")
	if rewriteRules && proxy.CACertPEM == "" {
		return fmt.Errorf("proxy: rewrite rules require a CA certificate")
	}

	// The rule set rides in a ConfigMap built by the caller; here we mount
	// it by name (deterministic per pod) — the caller creates the ConfigMap
	// via ProxyConfigMap.
	cmName := ProxyConfigMapName(workloadName)

	args := []string{"--mode=proxy",
		"--rules=/etc/easyproxy/rules.yaml",
		"--redir-addr=127.0.0.1:7893",
		"--dns-addr=127.0.0.1:7894",
		"--connect-addr=" + proxyConnectStr,
		"--bypass-cidrs=" + strings.Join(proxy.ClusterCIDRs, ","),
	}
	if proxy.CACertPEM != "" {
		args = append(args, "--ca-cert=/etc/easyproxy/ca/ca.crt", "--ca-key=/etc/easyproxy/ca/ca.key")
	}
	if proxy.MitmDefault {
		args = append(args, "--mitm-default")
	}

	proxyContainer := corev1.Container{
		Name:  "easyproxy",
		Image: proxy.Image,
		Args:  args,
		Ports: []corev1.ContainerPort{
			{Name: "redir", ContainerPort: proxyRedirPort},
			{Name: "dns", ContainerPort: proxyDNSPort},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("50m"),
				corev1.ResourceMemory: resource.MustParse("64Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("256Mi"),
			},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "easyproxy-rules", MountPath: "/etc/easyproxy", ReadOnly: true},
		},
	}

	initArgs := []string{"--mode=init",
		"--redir-addr=127.0.0.1:7893",
		"--dns-addr=127.0.0.1:7894",
		"--bypass-cidrs=" + strings.Join(proxy.ClusterCIDRs, ","),
	}
	if proxy.InterceptAllTCP {
		initArgs = append(initArgs, "--intercept-all-tcp")
	}
	initContainer := corev1.Container{
		Name:  "easyproxy-init",
		Image: proxy.Image,
		Args:  initArgs,
		SecurityContext: &corev1.SecurityContext{
			Privileged:   boolPtr(false),
			RunAsNonRoot: boolPtr(false), // iptables needs root inside the init netns
			Capabilities: &corev1.Capabilities{
				Add:  []corev1.Capability{"NET_ADMIN"},
				Drop: []corev1.Capability{"ALL"},
			},
		},
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("10m"),
				corev1.ResourceMemory: resource.MustParse("16Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("32Mi"),
			},
		},
	}

	vols := []corev1.Volume{{
		Name: "easyproxy-rules",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
			Items:                []corev1.KeyToPath{{Key: "rules.yaml", Path: "rules.yaml"}},
		}},
	}}
	proxyContainer.VolumeMounts = append(proxyContainer.VolumeMounts,
		corev1.VolumeMount{Name: "easyproxy-ca", MountPath: "/etc/easyproxy/ca", ReadOnly: true})
	if proxy.CACertPEM != "" {
		vols = append(vols, corev1.Volume{Name: "easyproxy-ca", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: ProxyCASecretName(workloadName)}}})
	}

	pod.InitContainers = append(pod.InitContainers, initContainer)
	pod.Containers = append(pod.Containers, proxyContainer)
	pod.Volumes = append(pod.Volumes, vols...)

	// The workload container must trust the MITM CA and prefer the explicit
	// proxy (same engine, better diagnostics than silent REDIRECT).
	for i := range pod.Containers {
		c := &pod.Containers[i]
		if c.Name == "easyproxy" {
			continue
		}
		c.Env = append(c.Env,
			corev1.EnvVar{Name: "HTTP_PROXY", Value: "http://" + proxyConnectStr},
			corev1.EnvVar{Name: "HTTPS_PROXY", Value: "http://" + proxyConnectStr},
			corev1.EnvVar{Name: "NO_PROXY", Value: "localhost,127.0.0.1,.svc,.svc.cluster.local," +
				strings.Join(proxy.ClusterCIDRs, ",")},
		)
		if proxy.CACertPEM != "" {
			c.Env = append(c.Env,
				corev1.EnvVar{Name: "SSL_CERT_FILE", Value: proxyCAPath},
				corev1.EnvVar{Name: "NODE_EXTRA_CA_CERTS", Value: proxyCAPath},
				corev1.EnvVar{Name: "REQUESTS_CA_BUNDLE", Value: proxyCAPath},
			)
			c.VolumeMounts = append(c.VolumeMounts,
				corev1.VolumeMount{Name: "easyproxy-ca", MountPath: proxyCAPath, SubPath: "ca.crt", ReadOnly: true})
		}
	}
	return nil
}

// ProxyConfigMapName is the deterministic ConfigMap name for a pod's rules.
func ProxyConfigMapName(podName string) string { return podName + "-easyproxy-rules" }

// ProxyCASecretName is the deterministic CA Secret name for a pod.
func ProxyCASecretName(podName string) string { return podName + "-easyproxy-ca" }

// CreateProxyConfigMap writes the rule set ConfigMap for a pod.
func (c *Client) CreateProxyConfigMap(ctx context.Context, podName, rules string) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: ProxyConfigMapName(podName), Namespace: c.namespace},
		Data:       map[string]string{"rules.yaml": rules},
	}
	_, err := c.cs.CoreV1().ConfigMaps(c.namespace).Create(ctx, cm, metav1.CreateOptions{})
	if isAlreadyExists(err) {
		_, err = c.cs.CoreV1().ConfigMaps(c.namespace).Update(ctx, cm, metav1.UpdateOptions{})
	}
	return err
}

// CreateProxyCASecret writes the CA cert+key Secret for a pod.
func (c *Client) CreateProxyCASecret(ctx context.Context, podName, certPEM, keyPEM string) error {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ProxyCASecretName(podName), Namespace: c.namespace},
		Data:       map[string][]byte{"ca.crt": []byte(certPEM), "ca.key": []byte(keyPEM)},
	}
	_, err := c.cs.CoreV1().Secrets(c.namespace).Create(ctx, sec, metav1.CreateOptions{})
	if isAlreadyExists(err) {
		_, err = c.cs.CoreV1().Secrets(c.namespace).Update(ctx, sec, metav1.UpdateOptions{})
	}
	return err
}
