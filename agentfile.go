package main

import (
	"context"
	"fmt"

	"github.com/abcp-sdk/abc-protocol-go/protocol"
)

// fetchAgentFile reads a stored file's bytes from the shared NATS file store
// (abc-protocol FileStore). Files are NOT owned by the agent — bytes live in
// the persistent object bucket keyed by code, metadata in the files.meta KV
// bucket, so any NATS member can read them directly. Used by sandbox-download
// to bring an uploaded attachment into the sandbox workspace.
func (s *server) fetchAgentFile(ctx context.Context, code string) ([]byte, error) {
	if s.bus == nil {
		return nil, fmt.Errorf("file store unavailable (nats not connected)")
	}
	store := protocol.NewFileStore(s.bus)
	rec, err := store.Get(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("file store get %q: %w", code, err)
	}
	if rec == nil || rec.Data == nil {
		return nil, fmt.Errorf("file not found: %s", code)
	}
	return rec.Data, nil
}
