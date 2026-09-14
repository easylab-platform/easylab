package main

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	easylabv1 "github.com/easylab-platform/easylab-proto/easylab/v1"
	"github.com/easylab-platform/easyvcs/store"
)

// TestServiceLaunchAuthz verifies LaunchService enforces maintainer+ on a
// repo-bound service (and requires auth for standalone).
func TestServiceLaunchAuthz(t *testing.T) {
	w := newRBACWorld(t)
	co := &connOps{w.s}
	// Developer on the repo: denied.
	_, err := co.LaunchService(principalCtx(w.dev), connect.NewRequest(&easylabv1.LaunchServiceRequest{
		Name: "svc", Image: "nginx", Org: "team", Repo: "pub",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("developer LaunchService: expected PermissionDenied, got %v", err)
	}
	// Anonymous standalone: unauthenticated.
	_, err = co.LaunchService(principalCtx(nil), connect.NewRequest(&easylabv1.LaunchServiceRequest{
		Name: "svc", Image: "nginx",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous standalone LaunchService: expected Unauthenticated, got %v", err)
	}
}

var _ = context.Background
var _ = store.RoleOwner
