package main

import (
	"context"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/easylab-platform/easylab/internal/ext/ops"
	"github.com/easylab-platform/easylab/internal/ext/repo"
)

// embedExtensions runs the ops + repo agent-tool extensions in-process (single
// binary) instead of launching separate containers. Both connect to NATS
// (the shared in-pod broker on loopback — all containers share the pod
// netns) and forward tool calls to the easylab gateway via easylab-sdk-go.
// They block until ctx is cancelled; failures are logged, not fatal.
func embedExtensions(ctx context.Context) {
	natsURL := os.Getenv("EASYLAB_EXT_NATS_URL")
	if natsURL == "" {
		natsURL = "nats://127.0.0.1:4222"
	}
	base := "http://127.0.0.1:8080"
	token := os.Getenv("EASYLAB_TOKEN")
	if token == "" {
		token = "devtoken"
	}
	agent := os.Getenv("EASYLAB_AGENT_URL")
	if agent == "" {
		agent = "http://127.0.0.1:8000"
	}

	log := slog.Default()
	waitReady(ctx, natsURL, 20*time.Second)

	go func() {
		if err := opsext.Run(ctx, opsext.Options{
			EasyLabURL: base,
			NATSURL:    natsURL,
			Token:      token,
		}); err != nil {
			log.Error("embedded ops-extension stopped", "err", err)
		}
	}()

	go func() {
		if err := repoext.Run(ctx, repoext.Options{
			Base:    base,
			Agent:   agent,
			NATSURL: natsURL,
			Token:   token,
		}); err != nil {
			log.Error("embedded repo-extension stopped", "err", err)
		}
	}()

	log.Info("embedded extensions starting", "nats", natsURL, "easylab", base)
}

// waitReady blocks until the given NATS endpoint accepts a TCP connection
// (optionally a host:port wrapped in the scheme), so the embedded extensions
// do not race the nats sidecar's startup in the shared pod netns.
func waitReady(ctx context.Context, natsURL string, timeout time.Duration) {
	hostport := natsURL
	if i := indexOfNatsURL(natsURL, "://"); i >= 0 {
		hostport = natsURL[i+3:]
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn, err := net.DialTimeout("tcp", hostport, time.Second)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func indexOfNatsURL(s, sep string) int {
	for i := 0; i+len(sep) <= len(s); i++ {
		if s[i:i+len(sep)] == sep {
			return i
		}
	}
	return -1
}
