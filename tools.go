package opsext

import (
	"github.com/abcp-sdk/abc-protocol-go/extension"
)

// handlers assembles the full tool map, delegating each domain to its own
// register function (sandbox_tools.go / build_tools.go / deploy_tools.go).
func (s *server) handlers() map[string]extension.ToolSpec {
	m := map[string]extension.ToolSpec{}
	s.registerSandboxTools(m)
	s.registerBuildTools(m)
	s.registerDeployTools(m)
	return m
}
