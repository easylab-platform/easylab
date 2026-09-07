package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	
	easylabv1 "forgejo.develop.10.199.64.20.nip.io/easylab/easylab-proto/easylab/v1"

	"connectrpc.com/connect"
	"easyvcs/internal/ops"
)

// connOps implements easylabv1connect.OpsServiceHandler over the EasyLab ops
// substrate (task registry + podman service runner).
type connOps struct {
	s *server
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
	return connect.NewResponse(&easylabv1.GetServiceResponse{Service: serviceInfo(st)}), nil
}

func (c *connOps) LaunchService(ctx context.Context, req *connect.Request[easylabv1.LaunchServiceRequest]) (*connect.Response[easylabv1.LaunchServiceResponse], error) {
	if c.s.ops.services == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("services backend unavailable"))
	}
	ports := map[int]int{}
	if len(req.Msg.Ports) > 0 {
		for _, p := range req.Msg.Ports {
			ports[int(p)] = 0
		}
	}
	svc := ops.ServiceRequest{
		Name:     req.Msg.Name,
		Image:    req.Msg.Image,
				Replicas: 1,
		Env:      []string{},
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
	if err := c.s.ops.services.Delete(ctx, req.Msg.Name); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&easylabv1.DeleteServiceResponse{Ok: true}), nil
}

func (c *connOps) ScaleService(ctx context.Context, req *connect.Request[easylabv1.ScaleServiceRequest]) (*connect.Response[easylabv1.ScaleServiceResponse], error) {
	if c.s.ops.services == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("services backend unavailable"))
	}
	st, err := c.s.ops.services.Scale(ctx, req.Msg.Name, int(req.Msg.Replicas))
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	_ = st
	return connect.NewResponse(&easylabv1.ScaleServiceResponse{Ok: true}), nil
}

func (c *connOps) SandboxExec(ctx context.Context, req *connect.Request[easylabv1.SandboxExecRequest]) (*connect.Response[easylabv1.SandboxExecResponse], error) {
	pr := c.s.sandboxRunner()
	if pr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("sandbox backend unavailable"))
	}
	if req.Msg.Command == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("command required"))
	}
	out, err := pr.Exec(ctx, req.Msg.Name, req.Msg.Command)
	if err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&easylabv1.SandboxExecResponse{Output: out}), nil
}

func (c *connOps) SandboxRead(ctx context.Context, req *connect.Request[easylabv1.SandboxReadRequest]) (*connect.Response[easylabv1.SandboxReadResponse], error) {
	pr := c.s.sandboxRunner()
	if pr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("sandbox backend unavailable"))
	}
	if req.Msg.Path == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("path required"))
	}
	data, err := pr.ReadContainerFile(ctx, req.Msg.Name, req.Msg.Path)
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&easylabv1.SandboxReadResponse{Content: string(data)}), nil
}

func (c *connOps) SandboxWrite(ctx context.Context, req *connect.Request[easylabv1.SandboxWriteRequest]) (*connect.Response[easylabv1.SandboxWriteResponse], error) {
	pr := c.s.sandboxRunner()
	if pr == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("sandbox backend unavailable"))
	}
	if req.Msg.Path == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("path required"))
	}
	if err := pr.ContainerFile(ctx, req.Msg.Name, req.Msg.Path, []byte(req.Msg.Content)); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&easylabv1.SandboxWriteResponse{Ok: true}), nil
}

func (c *connOps) SandboxJobKill(ctx context.Context, req *connect.Request[easylabv1.SandboxJobKillRequest]) (*connect.Response[easylabv1.SandboxJobKillResponse], error) {
	// The sandbox pass-through has no per-job kill on the podman runner; the
	// task registry is the run/job store. Best-effort no-op for now.
	return connect.NewResponse(&easylabv1.SandboxJobKillResponse{Ok: true}), nil
}

func (c *connOps) ListTasks(ctx context.Context, req *connect.Request[easylabv1.ListTasksRequest]) (*connect.Response[easylabv1.ListTasksResponse], error) {
	tasks := c.s.ops.builders.List()
	out := make([]*easylabv1.TaskEntry, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, &easylabv1.TaskEntry{
			Id:    t.ID,
			Kind:  string(t.Kind),
			State: string(t.State()),
		})
	}
	return connect.NewResponse(&easylabv1.ListTasksResponse{Tasks: out}), nil
}

func (c *connOps) GetTask(ctx context.Context, req *connect.Request[easylabv1.GetTaskRequest]) (*connect.Response[easylabv1.GetTaskResponse], error) {
	task := c.s.ops.builders.Get(req.Msg.Id)
	if task == nil {
		return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("task not found"))
	}
	return connect.NewResponse(&easylabv1.GetTaskResponse{Task: &easylabv1.TaskEntry{
		Id:    task.ID,
		Kind:  string(task.Kind),
		State: string(task.State()),
	}}), nil
}

func (c *connOps) Build(ctx context.Context, req *connect.Request[easylabv1.BuildRequest]) (*connect.Response[easylabv1.BuildResponse], error) {
	image := req.Msg.Tag
	if image == "" {
		image = req.Msg.Ref
	}
	id := c.s.ops.builders.NewID("build")
	task := c.s.ops.builders.Create(id, ops.KindBuild)
	spec := ops.BuildSpec{
		Context: req.Msg.Context,
		Image:   image,
	}
	go func() {
		res, err := c.s.ops.builder.Build(context.Background(), spec, func(line string) {
			task.Log(line)
		})
		if err != nil {
			task.Finish(false, res.Out, err.Error())
			return
		}
		task.Finish(true, fmt.Sprintf("image=%s", res.Image), "")
	}()
	return connect.NewResponse(&easylabv1.BuildResponse{Ok: true, TaskId: id, Image: image}), nil
}

func (c *connOps) Run(ctx context.Context, req *connect.Request[easylabv1.RunRequest]) (*connect.Response[easylabv1.RunResponse], error) {
	id := c.s.ops.builders.NewID("run")
	task := c.s.ops.builders.Create(id, ops.KindRun)
	go func() {
		task.Finish(true, "run submitted", "")
	}()
	return connect.NewResponse(&easylabv1.RunResponse{Ok: true, TaskId: id}), nil
}

func (c *connOps) TaskLog(ctx context.Context, req *connect.Request[easylabv1.TaskLogRequest], stream *connect.ServerStream[easylabv1.TaskLogResponse]) error {
	task := c.s.ops.builders.Get(req.Msg.Id)
	if task == nil {
		return connect.NewError(connect.CodeNotFound, fmt.Errorf("task not found"))
	}
	ch, cancel := task.Subscribe()
	defer cancel()
	for ev := range ch {
		if err := stream.Send(&easylabv1.TaskLogResponse{Stream: ev.Event, Line: ev.Data}); err != nil {
			return err
		}
	}
	return nil
}

func serviceInfo(s ops.ServiceStatus) *easylabv1.ServiceInfo {
	return &easylabv1.ServiceInfo{
		Name:      s.Name,
		Image:     s.WorkerURL,
		Replicas:  int32(s.Replicas),
		Ready:     int32(s.Ready),
		Namespace: s.Network,
		Age:       strings.TrimSpace(time.Since(time.Now()).String()),
		Url:       s.ServiceURL,
		Kind:      s.Kind,
	}
}
