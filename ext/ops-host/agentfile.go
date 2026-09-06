package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// fetchAgentFile downloads a stored file's bytes from the agent's file API
// (`GET /api/v1/files/{code}`). Used by sandbox-download to bring an uploaded
// attachment into the sandbox workspace. Auth is an optional Bearer token
// (AGENT_API_KEY), consistent with memory-extension.
func (s *server) fetchAgentFile(ctx context.Context, code string) ([]byte, error) {
	agentURL := envOr("AGENT_BASE_URL", envOr("ZERGX_AGENT_URL", "http://agent.zergx.svc.cluster.local:80"))
	u := strings.TrimRight(agentURL, "/") + "/api/v1/files/" + code
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if tok := envOr("AGENT_API_KEY", ""); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	// Use the long-lived client: large files must not be cut short by the
	// default 60s timeout.
	resp, err := longClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("agent files API HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	return data, nil
}
