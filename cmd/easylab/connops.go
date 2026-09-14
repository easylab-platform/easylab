package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"

	"connectrpc.com/connect"
	"github.com/easylab-platform/easylab/internal/k8s"
	"github.com/easylab-platform/easylab/internal/ops"
)

// connOps implements easylabv1connect.OpsServiceHandler over the EasyLab ops
// substrate (featured services + k8s service runner).
type connOps struct {
	s *server
}

// rewriteImage rewrites external image refs (docker.io, ghcr.io, ...) to pull
// through easylab's own OCI registry, which the node can reach. The node has
// no direct egress to public registries.
func (c *connOps) rewriteImage(ref string) string {
	if c.s.k8s == nil {
		return ref
	}
	return k8s.RewriteImageRef(ref, c.s.k8s.RegistryHost())
}

func (c *connOps) OpsStatus(ctx context.Context, req *connect.Request[easylabv1.OpsStatusRequest]) (*connect.Response[easylabv1.OpsStatusResponse], error) {
	return connect.NewResponse(&easylabv1.OpsStatusResponse{Ok: true, Version: "easylab-ops", Sandboxes: 0}), nil
}

func (c *connOps) ListNamespaces(ctx context.Context, req *connect.Request[easylabv1.ListNamespacesRequest]) (*connect.Response[easylabv1.ListNamespacesResponse], error) {
	var out []*easylabv1.NamespaceInfo
	for _, ns := range c.s.ops.namespaces.List() {
		out = append(out, &easylabv1.NamespaceInfo{Name: ns})
	}
	return connect.NewResponse(&easylabv1.ListNamespacesResponse{Namespaces: out}), nil
}

func (c *connOps) ListServices(ctx context.Context, req *connect.Request[easylabv1.ListServicesRequest]) (*connect.Response[easylabv1.ListServicesResponse], error) {
	if c.s.ops.services == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("services backend unavailable"))
	}
	st, err := c.s.ops.services.List(ctx, req.Msg.Namespace)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*easylabv1.ServiceInfo, 0, len(st))
	for _, s := range st {
		if !c.s.canSeeService(ctx, s.Owner, s.Org, s.Repo) {
			continue
		}
		out = append(out, serviceInfo(s))
	}
	return connect.NewResponse(&easylabv1.ListServicesResponse{Services: out}), nil
}

func (c *connOps) GetService(ctx context.Context, req *connect.Request[easylabv1.GetServiceRequest]) (*connect.Response[easylabv1.GetServiceResponse], error) {
	if c.s.ops.services == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("services backend unavailable"))
	}
	st, err := c.s.ops.services.Status(ctx, req.Msg.Name)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if !c.s.canSeeService(ctx, st.Owner, st.Org, st.Repo) {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("service not found"))
	}
	return connect.NewResponse(&easylabv1.GetServiceResponse{Service: serviceInfo(st)}), nil
}

func (c *connOps) LaunchService(ctx context.Context, req *connect.Request[easylabv1.LaunchServiceRequest]) (*connect.Response[easylabv1.LaunchServiceResponse], error) {
	// Authorization: a service bound to a repository requires maintainer+ on
	// that repo (deploying is a shared, externally-visible side effect, on par
	// with merging). A standalone service (no org/repo) is owned by the caller.
	if req.Msg.Org != "" || req.Msg.Repo != "" {
		if err := c.s.requireRepoAction(ctx, req.Msg.Org, req.Msg.Repo, repoCanMerge, "deploying a service"); err != nil {
			return nil, err
		}
	} else if err := requireAuthenticated(ctx, "deploying a service"); err != nil {
		return nil, err
	}
	if c.s.ops.services == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("services backend unavailable"))
	}
	ports := map[int]int{}
	for _, p := range req.Msg.Ports {
		if p != nil {
			ports[int(p.Container)] = int(p.Service)
		}
	}
	env := make([]string, 0, len(req.Msg.Env))
	for k, v := range req.Msg.Env {
		env = append(env, k+"="+v)
	}
	replicas := int(req.Msg.Replicas)
	if replicas == 0 {
		replicas = 1
	}
	annotations := req.Msg.Annotations
	if annotations == nil {
		annotations = map[string]string{}
	}
	// k8s label VALUES must be RFC-1123 (no ':'), so any label value (incl.
	// the ext-supplied easylab/session=org:repo:branch) is sanitized for
	// storage; filtering compares against the sanitized form.
	if req.Msg.Session != "" {
		annotations["easylab/session"] = k8s.LabelKey(req.Msg.Session)
	}
	if req.Msg.Org != "" {
		annotations["easylab/org"] = k8s.LabelKey(req.Msg.Org)
	}
	if req.Msg.Repo != "" {
		annotations["easylab/repo"] = k8s.LabelKey(req.Msg.Repo)
	}
	for k, v := range annotations {
		if k == "easylab/owner" {
			continue
		}
		annotations[k] = k8s.LabelKey(v)
	}
	// Ownership label: the deploying user (used to filter list/get/delete).
	annotations["easylab/owner"] = fmt.Sprintf("%d", principalOf(ctx).UserID)
	if req.Msg.Kind != "" {
		annotations["easylab/kind"] = k8s.LabelKey(req.Msg.Kind)
	}
	svc := ops.ServiceRequest{
		Name:        req.Msg.Name,
		Image:       c.rewriteImage(req.Msg.Image),
		Command:     req.Msg.Command,
		Ports:       ports,
		Env:         env,
		Replicas:    replicas,
		Group:       req.Msg.Group,
		Network:     req.Msg.Network,
		CPUs:        req.Msg.Cpus,
		MemoryBytes: req.Msg.MemoryBytes,
		Labels:      annotations,
	}
	st, err := c.s.ops.services.Launch(ctx, svc, nil)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&easylabv1.LaunchServiceResponse{Ok: true, Name: st.Name, Url: st.WorkerURL}), nil
}

func (c *connOps) DeleteService(ctx context.Context, req *connect.Request[easylabv1.DeleteServiceRequest]) (*connect.Response[easylabv1.DeleteServiceResponse], error) {
	if c.s.ops.services == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("services backend unavailable"))
	}
	st, err := c.s.ops.services.Status(ctx, req.Msg.Name)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if !c.s.canOperateService(ctx, st.Owner, st.Org, st.Repo) {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("deleting a service requires maintainer"))
	}
	if err := c.s.ops.services.Delete(ctx, req.Msg.Name); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.DeleteServiceResponse{Ok: true}), nil
}

func (c *connOps) ScaleService(ctx context.Context, req *connect.Request[easylabv1.ScaleServiceRequest]) (*connect.Response[easylabv1.ScaleServiceResponse], error) {
	if c.s.ops.services == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("services backend unavailable"))
	}
	st0, err := c.s.ops.services.Status(ctx, req.Msg.Name)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	if !c.s.canOperateService(ctx, st0.Owner, st0.Org, st0.Repo) {
		return nil, connect.NewError(connect.CodePermissionDenied, fmt.Errorf("scaling a service requires maintainer"))
	}
	st, err := c.s.ops.services.Scale(ctx, req.Msg.Name, int(req.Msg.Replicas))
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	_ = st
	return connect.NewResponse(&easylabv1.ScaleServiceResponse{Ok: true}), nil
}

func (c *connOps) SandboxExec(ctx context.Context, req *connect.Request[easylabv1.SandboxExecRequest]) (*connect.Response[easylabv1.SandboxExecResponse], error) {
	// Sandbox command execution goes through the worker API, not the ops
	// substrate. This REST-ish passthrough is deprecated in favor of the
	// SandboxService Execute RPC.
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("use SandboxService.Execute"))
}

func (c *connOps) SandboxRead(ctx context.Context, req *connect.Request[easylabv1.SandboxReadRequest]) (*connect.Response[easylabv1.SandboxReadResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("use SandboxService.FileRead"))
}

func (c *connOps) SandboxWrite(ctx context.Context, req *connect.Request[easylabv1.SandboxWriteRequest]) (*connect.Response[easylabv1.SandboxWriteResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("use SandboxService.FileWrite"))
}

func (c *connOps) SandboxJobKill(ctx context.Context, req *connect.Request[easylabv1.SandboxJobKillRequest]) (*connect.Response[easylabv1.SandboxJobKillResponse], error) {
	// Sandbox command execution goes through the worker API, not the ops
	// substrate. This legacy passthrough is deprecated in favor of
	// SandboxService.JobKill.
	return connect.NewResponse(&easylabv1.SandboxJobKillResponse{Ok: true}), nil
}

func serviceInfo(s ops.ServiceStatus) *easylabv1.ServiceInfo {
	return &easylabv1.ServiceInfo{
		Name:      s.Name,
		PodIp:     s.PodIP,
		Phase:     s.Phase,
		Image:     s.WorkerURL,
		Replicas:  int32(s.Replicas),
		Ready:     int32(s.Ready),
		Namespace: s.Network,
		Age:       strings.TrimSpace(time.Since(time.Now()).String()),
		Url:       s.ServiceURL,
		Kind:      s.Kind,
		Owner:     s.Owner,
		Org:       s.Org,
		Repo:      s.Repo,
		Session:   s.Session,
	}
}

// ---- Sync: repo snapshot into a service container ----

func (c *connOps) Sync(ctx context.Context, req *connect.Request[easylabv1.SyncRequest]) (*connect.Response[easylabv1.SyncResponse], error) {
	// Services are k8s Deployments now; pushing a repo tree into a service
	// container is no longer part of the model. Workspace sync for sandboxes
	// goes through SandboxService.SyncWorkspace (worker SyncFolder).
	return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("Sync: use SandboxService.SyncWorkspace"))
}
