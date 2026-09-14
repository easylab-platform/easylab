package main

import (
	"github.com/easylab-platform/easylab/internal/ops"
)

// opsState bundles all runtime/registry state the ops RPC handlers need.
type opsState struct {
	namespaces *ops.NamespaceRegistry
	services   ops.ServiceRunner
}

// newOpsState wires the ops substrate: container services run on Kubernetes
// (no privileged podman sidecar). The k8s client is created separately (main)
// so startup can degrade gracefully when not in-cluster.
func newOpsState() (*opsState, error) {
	return &opsState{
		namespaces: ops.FromEnv(),
	}, nil
}
