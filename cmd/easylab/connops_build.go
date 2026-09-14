package main

import (
	"context"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easylab/internal/k8s"
	"github.com/easylab-platform/easylab/internal/ops"
)

// Build launches a container image build asynchronously. It is the RPC form of
// the removed POST /api/v1/ops/builds.
func (c *connOps) Build(ctx context.Context, req *connect.Request[easylabv1.BuildRequest]) (*connect.Response[easylabv1.BuildResponse], error) {
	if err := requireAuthenticated(ctx, "launching a build"); err != nil {
		return nil, err
	}
	if req.Msg.Image == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("image required"))
	}
	if c.s.k8s == nil {
		return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("k8s backend unavailable"))
	}
	id := c.s.ops.builders.NewID("build")
	task := c.s.ops.builders.Create(id, ops.KindBuild)
	buildArgs := map[string]string{}
	for _, a := range req.Msg.BuildArgs {
		if i := strings.Index(a, "="); i > 0 {
			buildArgs[a[:i]] = a[i+1:]
		}
	}
	go func() {
		err := c.s.k8s.BuildImage(context.Background(), k8s.BuildOptions{
			ContextDir: req.Msg.Context, Dockerfile: req.Msg.Dockerfile, Image: req.Msg.Image,
			BuildArgs: buildArgs, NoCache: req.Msg.NoCache,
		}, func(line string) { task.Log(line) })
		if err != nil {
			task.Finish(false, "", err.Error())
			return
		}
		task.Finish(true, "image="+req.Msg.Image, "")
	}()
	return connect.NewResponse(&easylabv1.BuildResponse{Ok: true, BuildId: id}), nil
}
