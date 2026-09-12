package k8s

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// SandboxSpec describes a worker sandbox workload.
type SandboxSpec struct {
	Name      string
	Image     string            // worker image (prebuilt, or base+worker derived)
	Workspace string            // default /workspace
	Env       map[string]string // extra env (WORKER_TOKEN added by caller)
	Command   []string          // entrypoint override
	// InitWorker, when set, is an image carrying the static worker binary; an
	// init container copies it into a shared emptyDir (used to inject the
	// worker into an arbitrary base image without building a derived image).
	InitWorker   string
	WorkerBinSrc string // path of the binary inside InitWorker (default /usr/local/bin/easyworker)
	DeviceLimits map[string]string
	NodeSelector map[string]string
	NeedsTun     bool
	WorkerPort   int32
}

// SandboxStatus is a live sandbox view.
type SandboxStatus struct {
	Name    string
	Phase   string
	PodIP   string
	Ready   bool
	Service string
}

func (c *Client) port(s SandboxSpec) int32 {
	if s.WorkerPort != 0 {
		return s.WorkerPort
	}
	return 48080
}

// LaunchSandbox creates a Pod + ClusterIP Service for a worker sandbox.
func (c *Client) LaunchSandbox(ctx context.Context, s SandboxSpec) (SandboxStatus, error) {
	port := c.port(s)
	workspace := s.Workspace
	if workspace == "" {
		workspace = "/workspace"
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: c.namespace,
			Labels: map[string]string{"easylab/sandbox": "1", "app": s.Name}},
		Spec: corev1.PodSpec{
			RestartPolicy:    corev1.RestartPolicyAlways,
			NodeSelector:     s.NodeSelector,
			ImagePullSecrets: c.pullSecrets(),
		},
	}
	env := []corev1.EnvVar{
		{Name: "WORKER_WORKSPACE", Value: workspace},
		{Name: "WORKER_PORT", Value: strconv.Itoa(int(port))},
	}
	for k, v := range s.Env {
		env = append(env, corev1.EnvVar{Name: k, Value: v})
	}
	main := corev1.Container{
		Name: "worker", Image: s.Image, Env: env,
		Ports:     []corev1.ContainerPort{{Name: "worker", ContainerPort: port}},
		Resources: defaultResources(),
	}
	if len(s.Command) > 0 {
		main.Command = s.Command
	}
	if len(s.DeviceLimits) > 0 {
		lim := corev1.ResourceList{}
		for k, v := range s.DeviceLimits {
			lim[corev1.ResourceName(k)] = resource.MustParse(v)
		}
		main.Resources.Limits = lim
	}
	var vols []corev1.Volume
	var mounts []corev1.VolumeMount
	if s.InitWorker != "" {
		src := s.WorkerBinSrc
		if src == "" {
			src = "/usr/local/bin/easyworker"
		}
		vols = append(vols, corev1.Volume{Name: "worker-bin", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "worker-bin", MountPath: "/easylab-worker"})
		pod.Spec.InitContainers = []corev1.Container{{
			Name: "inject-worker", Image: s.InitWorker,
			Command:      []string{"/bin/sh", "-c", "cp " + src + " /easylab-worker/easyworker && chmod +x /easylab-worker/easyworker"},
			VolumeMounts: []corev1.VolumeMount{{Name: "worker-bin", MountPath: "/easylab-worker"}},
		}}
		if len(main.Command) == 0 {
			main.Command = []string{"/easylab-worker/easyworker"}
		}
	}
	if s.NeedsTun {
		vols = append(vols, corev1.Volume{Name: "devtun", VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: "/dev/net/tun", Type: hpPtr(corev1.HostPathCharDev)}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "devtun", MountPath: "/dev/net/tun"})
	}
	main.VolumeMounts = append(main.VolumeMounts, mounts...)
	pod.Spec.Volumes = vols
	pod.Spec.Containers = []corev1.Container{main}

	if _, err := c.cs.CoreV1().Pods(c.namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		return SandboxStatus{}, fmt.Errorf("create pod: %w", err)
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: c.namespace, Labels: map[string]string{"app": s.Name}},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": s.Name},
			Ports:    []corev1.ServicePort{{Name: "worker", Port: port, TargetPort: intstr.FromInt32(port)}},
		},
	}
	if _, err := c.cs.CoreV1().Services(c.namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		return SandboxStatus{}, fmt.Errorf("create service: %w", err)
	}
	return c.Status(ctx, s.Name)
}

// Status returns a sandbox's live status.
func (c *Client) Status(ctx context.Context, name string) (SandboxStatus, error) {
	p, err := c.cs.CoreV1().Pods(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return SandboxStatus{}, err
	}
	st := SandboxStatus{Name: name, Phase: string(p.Status.Phase), PodIP: p.Status.PodIP, Service: c.ServiceDNS(name)}
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			st.Ready = true
		}
	}
	return st, nil
}

// WaitSandboxReady polls the worker health endpoint on the pod IP.
func (c *Client) WaitSandboxReady(ctx context.Context, name string, port int32, timeout time.Duration) error {
	if port == 0 {
		port = 48080
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p, err := c.cs.CoreV1().Pods(c.namespace).Get(ctx, name, metav1.GetOptions{})
		if err == nil && p.Status.Phase == corev1.PodRunning && p.Status.PodIP != "" {
			if dialHealth(ctx, p.Status.PodIP, port) == nil {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("sandbox %s not healthy in %s", name, timeout)
}

// DeleteSandbox removes the pod + service.
func (c *Client) DeleteSandbox(ctx context.Context, name string) error {
	_ = c.cs.CoreV1().Services(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	err := c.cs.CoreV1().Pods(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// ListSandboxes lists easylab-managed worker pods.
func (c *Client) ListSandboxes(ctx context.Context) ([]SandboxStatus, error) {
	pods, err := c.cs.CoreV1().Pods(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: "easylab/sandbox=1"})
	if err != nil {
		return nil, err
	}
	out := make([]SandboxStatus, 0, len(pods.Items))
	for i := range pods.Items {
		p := &pods.Items[i]
		ready := false
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Ready {
				ready = true
			}
		}
		out = append(out, SandboxStatus{Name: p.Name, Phase: string(p.Status.Phase), PodIP: p.Status.PodIP, Ready: ready, Service: c.ServiceDNS(p.Name)})
	}
	return out, nil
}

func (c *Client) pullSecrets() []corev1.LocalObjectReference {
	if c.imagePullSecret == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: c.imagePullSecret}}
}

// dialHealth opens a TCP connection to the worker port (health probe).
func dialHealth(ctx context.Context, ip string, port int32) error {
	d := net.Dialer{Timeout: time.Second}
	conn, err := d.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return err
	}
	return conn.Close()
}

var _ = bytes.MinRead
