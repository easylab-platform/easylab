package main

import (
	"context"
	"fmt"
)

// fetchAgentFile downloads a stored file's bytes from the abc agent via the
// typed agent-sdk (AgentService.GetFile). Used by sandbox-download to bring an
// uploaded attachment into the sandbox workspace.
func (s *server) fetchAgentFile(ctx context.Context, code string) ([]byte, error) {
	data, _, _, err := s.agent.GetFile(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("agent files API: %w", err)
	}
	return data, nil
}
