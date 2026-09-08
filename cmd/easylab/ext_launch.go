package main

import (
	"context"
	"log"
	"net"
	"os"

	"github.com/easylab-platform/easylab/internal/ops"
)

// launchExtensions starts the ops + repo extension containers on boot, so the
// abc agent can discover their tools over NATS. It mirrors a client calling
// OpsService/LaunchService, but runs in-process against the podman sidecar via
// the shared service runner. Images come from the internal registry
// (EASYVCS_REGISTRY, no TLS). Extensions are optional: a failure to start one
// logs a warning and does not abort easylab.
func launchExtensions(s *server, st *opsState) {
	if os.Getenv("EASYLAB_EXT_DISABLE") == "1" {
		return
	}
	if st == nil || st.services == nil {
		log.Println("ext-launch: services backend unavailable; skipping")
		return
	}
	// NATS is served in-process in the same pod; the ext containers reach it via
	// the pod IP (they live on the podman bridge network, not the pod netns).
	natsURL := os.Getenv("EASYLAB_EXT_NATS_URL")
	if natsURL == "" {
		if ip := podIP(); ip != "" {
			natsURL = "nats://" + ip + ":4222"
		}
	}
	registry := registryHost()
	type ext struct {
		name  string
		image string
		port  string
	}
	exts := []ext{
		{"ops-ext", registry + "/easylab/ext-ops:20260906170000", "18092"},
		{"repo-ext", registry + "/easylab/ext-repo:20260906170000", "18093"},
	}
	for _, e := range exts {
		req := ops.ServiceRequest{
			Name:     e.name,
			Image:    e.image,
			Replicas: 1,
			Restart:  "unless-stopped",
			Env: []string{
				"NATS_URL=" + natsURL,
				"PORT=" + e.port,
			},
		}
		// Idempotent: if it's already running (informal status) skip re-create.
		if st, err := st.services.Status(context.Background(), e.name); err == nil && st.Name == e.name {
			continue
		}
		res, err := st.services.Launch(context.Background(), req, func(string) {})
		if err != nil {
			log.Printf("ext-launch: %s failed: %v", e.name, err)
			continue
		}
		log.Printf("ext-launch: started %s (image %s, nats %s)", res.Name, e.image, natsURL)
	}
}

// podIP returns the pod's IP as seen from inside the container (eth0), used as
// the NATS address the ext containers can reach. Prefer the POD_IP env (set by
// the k8s downward API when available); otherwise sniff the first non-loopback
// IPv4.
func podIP() string {
	if ip := os.Getenv("POD_IP"); ip != "" {
		return ip
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
			if v4 := ipn.IP.To4(); v4 != nil {
				return v4.String()
			}
		}
	}
	return ""
}

