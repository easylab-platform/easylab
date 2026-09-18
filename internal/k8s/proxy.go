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
// EasySidecar intercepts the Pod's outbound 80/443 (plus DNS) and applies the
// block/direct/rewrite rule set. Nil on the workload spec means no proxy —
// the Pod is generated exactly as before.
type ProxySpec struct {
	// Rules is the YAML rule set text (see easysidecar README for the schema).
	Rules string
	// MitmDefault decrypts every intercepted TLS connection, not just
	// rewrite rules. Off by default.
	MitmDefault bool
	// CACertPEM is the MITM CA certificate distributed to the workload
	// (SSL_CERT_FILE / NODE_EXTRA_CA_CERTS). Required when any rewrite rule
	// is present.
	CACertPEM string
	// CAKeyPEM is the MITM CA private key (EasySidecar signs leaf certificates
	// with it). Namespace-scoped secret material.
	CAKeyPEM string
	// Image is the EasySidecar image. Empty uses the
	// chart default wired by the caller.
	Image string

	// DNS-spoof is the only interception mode: the Pod's resolver points at
	// the sidecar, which then listens on :53/:443/:80 directly — no netfilter,
	// no NET_ADMIN, no init container.
	//
	// SpoofUpstreamDNS is the cluster resolver (CoreDNS service IP) real
	// queries are forwarded to.
	SpoofUpstreamDNS string
	// UpstreamProxy is an optional egress HTTP proxy (mihomo) used for
	// DIRECT traffic.
	UpstreamProxy string
	// SelfIP optionally pins the address the sidecar answers for rewritten
	// names (defaults to the Pod IP via the downward API).
	SelfIP string
	// ClusterDomain is the cluster DNS domain for the search path
	// (default "cluster.local").
	ClusterDomain string
}

// proxyCAPath is where the sidecar mounts the MITM CA and where it is handed
// to the workload container's trust env.
const (
	proxyCAPath = "/etc/easysidecar/ca.crt"
)

// withProxy injects the EasySidecar sidecar, volumes, and env into a Pod spec.
// It appends to the existing containers; call it BEFORE the pod is created.
// proxy == nil is a no-op.
func withProxy(pod *corev1.PodSpec, workloadName, namespace string, proxy *ProxySpec) error {
	if proxy == nil {
		return nil
	}
	if proxy.Image == "" {
		return fmt.Errorf("proxy: image required")
	}
	if strings.TrimSpace(proxy.Rules) == "" {
		return fmt.Errorf("proxy: rules required")
	}
	if strings.TrimSpace(proxy.SpoofUpstreamDNS) == "" {
		return fmt.Errorf("proxy: SpoofUpstreamDNS required (cluster resolver)")
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
		"--rules=/etc/easysidecar/rules.yaml",
		"--spoof",
		"--spoof-dns-addr=0.0.0.0:53",
		"--spoof-tls-addr=0.0.0.0:443",
		"--spoof-http-addr=0.0.0.0:80",
		"--upstream-dns=" + proxy.SpoofUpstreamDNS,
	}
	if proxy.UpstreamProxy != "" {
		args = append(args, "--upstream-proxy="+proxy.UpstreamProxy)
	}
	if proxy.CACertPEM != "" {
		args = append(args, "--ca-cert=/etc/easysidecar/ca/ca.crt", "--ca-key=/etc/easysidecar/ca/ca.key")
	}
	if proxy.MitmDefault {
		args = append(args, "--mitm-default")
	}

	proxyContainer := corev1.Container{
		Name:  "easysidecar",
		Image: proxy.Image,
		Args:  args,
		Ports: []corev1.ContainerPort{
			{Name: "dns", ContainerPort: 53, Protocol: corev1.ProtocolUDP},
			{Name: "dns-tcp", ContainerPort: 53, Protocol: corev1.ProtocolTCP},
			{Name: "tls", ContainerPort: 443},
			{Name: "http", ContainerPort: 80},
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
			{Name: "easysidecar-rules", MountPath: "/etc/easysidecar", ReadOnly: true},
		},
	}

	vols := []corev1.Volume{{
		Name: "easysidecar-rules",
		VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
			Items:                []corev1.KeyToPath{{Key: "rules.yaml", Path: "rules.yaml"}},
		}},
	}}
	proxyContainer.VolumeMounts = append(proxyContainer.VolumeMounts,
		corev1.VolumeMount{Name: "easysidecar-ca", MountPath: "/etc/easysidecar/ca", ReadOnly: true})
	if proxy.CACertPEM != "" {
		vols = append(vols, corev1.Volume{Name: "easysidecar-ca", VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: ProxyCASecretName(workloadName)}}})
	}

	pod.Containers = append(pod.Containers, proxyContainer)
	pod.Volumes = append(pod.Volumes, vols...)

	// Point the Pod's resolver at the sidecar (dns-spoof). dnsPolicy=None
	// is required: with a policy, kubelet APPENDS dnsConfig.nameservers to
	// the cluster resolvers, so the client would use CoreDNS first and
	// never reach the sidecar. With None, ONLY our list is used — so we
	// must supply the search path and ndots ourselves.
	pod.DNSPolicy = corev1.DNSNone
	if pod.DNSConfig == nil {
		pod.DNSConfig = &corev1.PodDNSConfig{}
	}
	domain := proxy.ClusterDomain
	if domain == "" {
		domain = "cluster.local"
	}
	pod.DNSConfig.Nameservers = []string{"127.0.0.1"}
	pod.DNSConfig.Searches = []string{namespace + ".svc." + domain, "svc." + domain, domain}
	ndots := "5"
	pod.DNSConfig.Options = []corev1.PodDNSConfigOption{{Name: "ndots", Value: &ndots}}
	// POD_IP for the sidecar's -self-ip.
	for i := range pod.Containers {
		if pod.Containers[i].Name == "easysidecar" {
			pod.Containers[i].Env = append(pod.Containers[i].Env, corev1.EnvVar{
				Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{
					FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
				}})
		}
	}

	// The workload container must trust the MITM CA and prefer the explicit
	// same engine, better diagnostics than silent REDIRECT.
	for i := range pod.Containers {
		c := &pod.Containers[i]
		if c.Name == "easysidecar" {
			continue
		}
		if proxy.CACertPEM != "" {
			c.Env = append(c.Env,
				corev1.EnvVar{Name: "SSL_CERT_FILE", Value: proxyCAPath},
				corev1.EnvVar{Name: "NODE_EXTRA_CA_CERTS", Value: proxyCAPath},
				corev1.EnvVar{Name: "REQUESTS_CA_BUNDLE", Value: proxyCAPath},
			)
			c.VolumeMounts = append(c.VolumeMounts,
				corev1.VolumeMount{Name: "easysidecar-ca", MountPath: proxyCAPath, SubPath: "ca.crt", ReadOnly: true})
		}
	}
	return nil
}

// ProxyConfigMapName is the deterministic ConfigMap name for a pod's rules.
func ProxyConfigMapName(podName string) string { return podName + "-easysidecar-rules" }

// ProxyCASecretName is the deterministic CA Secret name for a pod.
func ProxyCASecretName(podName string) string { return podName + "-easysidecar-ca" }

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
