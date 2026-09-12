package ops

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/easylab-platform/easylab/internal/k8s"
)

// K8sServiceRunner implements ServiceRunner on Kubernetes: every service is a
// Deployment + Service in the target namespace. It replaces the former
// privileged podman runner.
type K8sServiceRunner struct {
	c *k8s.Client
}

// NewK8sServiceRunner wraps a k8s client as a ServiceRunner.
func NewK8sServiceRunner(c *k8s.Client) *K8sServiceRunner { return &K8sServiceRunner{c: c} }

// Name implements ServiceRunner.
func (r *K8sServiceRunner) Name() string { return "k8s" }

// Launch implements ServiceRunner.
func (r *K8sServiceRunner) Launch(ctx context.Context, req ServiceRequest, log func(string)) (ServiceStatus, error) {
	if req.Name == "" || req.Image == "" {
		return ServiceStatus{}, fmt.Errorf("name and image required")
	}
	ports := map[int32]int32{}
	for cp, sp := range req.Ports {
		ports[int32(cp)] = int32(sp)
	}
	labels := map[string]string{}
	for k, v := range req.Labels {
		labels[k] = v
	}
	st, err := r.c.LaunchService(ctx, k8s.ServiceSpec{
		Name:     req.Name,
		Image:    req.Image,
		Command:  commandArgs(req.Command),
		Env:      envVars(req.Env),
		Ports:    ports,
		Replicas: int32(req.Replicas),
		Labels:   labels,
	})
	if err != nil {
		return ServiceStatus{}, err
	}
	if log != nil {
		log("launch " + req.Name)
	}
	return r.toStatus(st, req), nil
}

// Status implements ServiceRunner.
func (r *K8sServiceRunner) Status(ctx context.Context, name string) (ServiceStatus, error) {
	st, err := r.c.ServiceStatus(ctx, name)
	if err != nil {
		return ServiceStatus{}, err
	}
	return r.toStatus(st, ServiceRequest{Name: name}), nil
}

// Delete implements ServiceRunner.
func (r *K8sServiceRunner) Delete(ctx context.Context, name string) error {
	return r.c.DeleteService(ctx, name)
}

// Scale implements ServiceRunner.
func (r *K8sServiceRunner) Scale(ctx context.Context, name string, replicas int) (ServiceStatus, error) {
	st, err := r.c.ScaleService(ctx, name, int32(replicas))
	if err != nil {
		return ServiceStatus{}, err
	}
	return r.toStatus(st, ServiceRequest{Name: name, Replicas: replicas}), nil
}

// List implements ServiceRunner.
func (r *K8sServiceRunner) List(ctx context.Context, network string) ([]ServiceStatus, error) {
	// k8s equivalents are labeled; the sandbox surface is listed via the
	// SandboxService. Services are enumerated through the k8s client.
	list, err := r.c.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ServiceStatus, 0, len(list))
	for _, s := range list {
		out = append(out, r.toStatus(s, ServiceRequest{Name: s.Name}))
	}
	return out, nil
}

func (r *K8sServiceRunner) toStatus(st k8s.SandboxStatus, req ServiceRequest) ServiceStatus {
	reps := req.Replicas
	if reps <= 0 {
		reps = 1
	}
	ready := 0
	if st.Ready {
		ready = reps
	}
	phase := st.Phase
	if phase == "" {
		phase = "Pending"
	}
	return ServiceStatus{
		Name: req.Name, Kind: "deployment", Replicas: reps, Ready: ready,
		Phase: phase, PodIP: st.PodIP, ServiceURL: st.Service,
		WorkerURL: "http://" + st.Service,
	}
}

// commandArgs maps a "sh -c <cmd>" style command onto k8s command+args.
func commandArgs(command string) []string {
	if command == "" {
		return nil
	}
	if strings.HasPrefix(command, "sh -c ") {
		return []string{"sh", "-c", strings.TrimPrefix(command, "sh -c ")}
	}
	return []string{"sh", "-c", command}
}

// envVars converts "KEY=VAL" entries to k8s env vars.
func envVars(env []string) []corev1.EnvVar {
	out := make([]corev1.EnvVar, 0, len(env))
	for _, kv := range env {
		if i := strings.Index(kv, "="); i > 0 {
			out = append(out, corev1.EnvVar{Name: kv[:i], Value: kv[i+1:]})
		}
	}
	return out
}
