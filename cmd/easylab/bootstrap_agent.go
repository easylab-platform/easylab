package main

import (
	"log"
	"os"
)

// bootstrapAgentBinding binds the bootstrap operator (the deployment's default
// identity) to its abcp-agent tenant in the DB, so the binding is a
// first-class record.
//
// The agent provisions the default tenant from AGENT_BOOTSTRAP_TENANT /
// AGENT_BOOTSTRAP_TOKEN (see the chart). The gateway presents that same token
// on the operator's forwarded agent RPCs. Env consumed here:
//
//	EASYLAB_AGENT_URL       — agent base URL (unset ⇒ binds nothing)
//	EASYLAB_AGENT_TENANT    — operator's agent tenant id (default "default")
//	EASYLAB_AGENT_TOKEN     — the agent credential for that tenant
//
// Idempotent: it only writes fields that are currently empty.
func (s *server) bootstrapAgentBinding() {
	if os.Getenv("EASYLAB_AGENT_URL") == "" {
		return
	}
	agentTenant := envOrStr("EASYLAB_AGENT_TENANT", "default")
	if agentTenant == "" || s.agentFwdToken == "" {
		return
	}
	// Bind the bootstrap operator; with no operator user (e.g. tokens seeded
	// out of band) there is nothing to bind.
	u, err := s.cs.GetUserByUsername(envOrStr("EASYLAB_BOOTSTRAP_USER", "operator"))
	if err != nil {
		return
	}
	if u.AgentTenant == agentTenant && u.AgentToken != "" {
		return
	}
	if err := s.cs.SetAgentBinding(u.ID, agentTenant, s.agentFwdToken); err != nil {
		log.Printf("bootstrap agent binding: %v", err)
	}
}
