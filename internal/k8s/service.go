package k8s

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ServiceSpec describes a deployment-backed service.
type ServiceSpec struct {
	Name         string
	Image        string
	Command      []string
	Env          []corev1.EnvVar
	Ports        map[int32]int32 // containerPort -> servicePort
	Replicas     int32
	Labels       map[string]string
	DeviceLimits map[string]string
	NodeSelector map[string]string
	NeedsTun     bool
}

// LaunchService creates/updates a Deployment + Service.
func (c *Client) LaunchService(ctx context.Context, s ServiceSpec) (SandboxStatus, error) {
	reps := s.Replicas
	if reps <= 0 {
		reps = 1
	}
	labels := s.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	labels["app"] = s.Name
	labels["easylab/service"] = "1"

	container := corev1.Container{Name: "svc", Image: s.Image, Env: s.Env, Resources: defaultResources()}
	if len(s.Command) > 0 {
		container.Command = s.Command
	}
	if len(s.DeviceLimits) > 0 {
		lim := corev1.ResourceList{}
		for k, v := range s.DeviceLimits {
			lim[corev1.ResourceName(k)] = resource.MustParse(v)
		}
		container.Resources.Limits = lim
	}
	for cp := range s.Ports {
		container.Ports = append(container.Ports, corev1.ContainerPort{ContainerPort: cp})
	}
	var vols []corev1.Volume
	var mounts []corev1.VolumeMount
	if s.NeedsTun {
		vols = append(vols, corev1.Volume{Name: "devtun", VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: "/dev/net/tun", Type: hpPtr(corev1.HostPathCharDev)}}})
		mounts = append(mounts, corev1.VolumeMount{Name: "devtun", MountPath: "/dev/net/tun"})
	}
	container.VolumeMounts = mounts

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: c.namespace, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &reps,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": s.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector:     s.NodeSelector,
					ImagePullSecrets: c.pullSecrets(),
					Volumes:          vols,
					Containers:       []corev1.Container{container},
				},
			},
		},
	}
	if _, err := c.cs.AppsV1().Deployments(c.namespace).Create(ctx, dep, metav1.CreateOptions{}); err != nil {
		if isAlreadyExists(err) {
			existing, gerr := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, s.Name, metav1.GetOptions{})
			if gerr != nil {
				return SandboxStatus{}, gerr
			}
			dep.ResourceVersion = existing.ResourceVersion
			if _, uerr := c.cs.AppsV1().Deployments(c.namespace).Update(ctx, dep, metav1.UpdateOptions{}); uerr != nil {
				return SandboxStatus{}, fmt.Errorf("update deployment: %w", uerr)
			}
		} else {
			return SandboxStatus{}, fmt.Errorf("create deployment: %w", err)
		}
	}

	svcPorts := []corev1.ServicePort{}
	for cp, sp := range s.Ports {
		if sp == 0 {
			sp = cp
		}
		svcPorts = append(svcPorts, corev1.ServicePort{Name: fmt.Sprintf("p%d", cp), Port: sp, TargetPort: intstr.FromInt32(cp)})
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: s.Name, Namespace: c.namespace, Labels: labels},
		Spec:       corev1.ServiceSpec{Selector: map[string]string{"app": s.Name}, Ports: svcPorts},
	}
	if _, err := c.cs.CoreV1().Services(c.namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil && !isAlreadyExists(err) {
		return SandboxStatus{}, fmt.Errorf("create service: %w", err)
	}
	return c.ServiceStatus(ctx, s.Name)
}

// ServiceStatus returns a deployment-backed service's live status.
func (c *Client) ServiceStatus(ctx context.Context, name string) (SandboxStatus, error) {
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return SandboxStatus{}, err
	}
	st := SandboxStatus{Name: name, Phase: "Running", Service: c.ServiceDNS(name)}
	if d.Status.ReadyReplicas > 0 {
		st.Ready = true
	}
	return st, nil
}

// DeleteService removes the Deployment + Service.
func (c *Client) DeleteService(ctx context.Context, name string) error {
	_ = c.cs.CoreV1().Services(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	err := c.cs.AppsV1().Deployments(c.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

// ScaleService sets replicas.
func (c *Client) ScaleService(ctx context.Context, name string, replicas int32) (SandboxStatus, error) {
	d, err := c.cs.AppsV1().Deployments(c.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return SandboxStatus{}, err
	}
	d.Spec.Replicas = &replicas
	if _, err := c.cs.AppsV1().Deployments(c.namespace).Update(ctx, d, metav1.UpdateOptions{}); err != nil {
		return SandboxStatus{}, err
	}
	return c.ServiceStatus(ctx, name)
}

func isAlreadyExists(err error) bool {
	return err != nil && (containsStr(err.Error(), "already exists"))
}

func containsStr(s, sub string) bool { return stringsContains(s, sub) }

// ListServices lists easylab-managed service deployments (label easylab/service=1).
func (c *Client) ListServices(ctx context.Context) ([]SandboxStatus, error) {
	list, err := c.cs.AppsV1().Deployments(c.namespace).List(ctx, metav1.ListOptions{LabelSelector: "easylab/service=1"})
	if err != nil {
		return nil, err
	}
	out := make([]SandboxStatus, 0, len(list.Items))
	for i := range list.Items {
		d := &list.Items[i]
		out = append(out, SandboxStatus{Name: d.Name, Phase: "Running", Ready: d.Status.ReadyReplicas > 0, Service: c.ServiceDNS(d.Name)})
	}
	return out, nil
}
