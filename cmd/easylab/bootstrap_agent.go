package main

import (
	"log"
	"os"
)

// bootstrapAgentTenant binds the DEFAULT tenant (id 1) to its abcp-agent
// counterpart in the DB, so the binding is a first-class record rather than an
// env-var special case in agentTokenForTenant.
//
// The agent provisions the default tenant from AGENT_BOOTSTRAP_TENANT /
// AGENT_BOOTSTRAP_TOKEN (see the chart). The gateway presents that same token
// on the default tenant's forwarded agent RPCs. Env consumed here:
//
//	EASYLAB_AGENT_URL         — agent base URL (unset ⇒ binds nothing)
//	EASYLAB_AGENT_TENANT      — default tenant's agent id (default "default")
//	EASYLAB_AGENT_TOKEN       — the agent credential for that tenant
//
// Idempotent: it only writes fields that are currently empty.
func (s *server) bootstrapAgentTenant() {
	if os.Getenv("EASYLAB_AGENT_URL") == "" {
		return
	}
	agentTenant := envOrStr("EASYLAB_AGENT_TENANT", "default")
	if agentTenant == "" || s.agentFwdToken == "" {
		return
	}
	t, err := s.cs.GetTenant(1)
	if err != nil {
		log.Printf("bootstrap default agent binding: %v", err)
		return
	}
	if t.AgentTenant == agentTenant && t.AgentToken != "" {
		return
	}
	if err := s.cs.SetAgentBinding(1, agentTenant, s.agentFwdToken); err != nil {
		log.Printf("bootstrap default agent binding: %v", err)
	}
}
