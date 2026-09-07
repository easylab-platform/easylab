package main

import (
	easylabsdk "github.com/easylab-platform/easylab-client-sdk"
	"context"
	_ "embed"
	"log/slog"
	"os"
	"path/filepath"
	"os/signal"
	"syscall"
	"time"

	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
	"github.com/abcp-sdk/abc-protocol-go/extension"
	"github.com/abcp-sdk/abc-protocol-go/manifest"
	natsbus "github.com/abcp-sdk/abc-protocol-go/transport/nats"
)

//go:embed manifest.yaml
var manifestYaml []byte

// server wires the two faces of repo-extension:
//
//   - tool face (NATS): agent file/git tools forwarding to easylab, with
//     the (org, repo, bookmark) triple resolved from the injected `_session`
//     via the mapping table;
//   - workspace face: lifecycle events from the agent (durable NATS
//     subscription) eagerly mirrored into easylab branches + mapping rows.
type server struct {
	base  string // easylab base URL
	agent string // agent-ts base URL
	store *Store
	cache *sessCache
	lab   *easylabClient
	sdk   *easylabsdk.Client // typed easylab client (search/graph/compare/rebase/tree/revisions)
	ag    *agentClient
	ext   *extension.Extension
}

func main() {
	log := slog.Default().With("svc", "repo-extension")
	s := &server{
		base:  envOr("EASYLAB_URL", "http://127.0.0.1:18160"),
		agent: envOr("AGENT_URL", "http://agent.easylab.svc.cluster.local:80"),
	}
	s.lab = newClient(s.base, envOr("EASYLAB_TOKEN", "devtoken"))
	s.sdk = easylabsdk.New(s.base, envOr("EASYLAB_TOKEN", "devtoken"))
	s.ag = newAgentClient(s.agent)
	s.cache = newSessCache(5 * time.Second)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := OpenStore(ctx, pgConfig())
	if err != nil {
		log.Error("pg connect failed", "err", err)
		os.Exit(1)
	}
	s.store = store
	defer store.Close()

	natsURL := envOr("NATS_URL", "nats://127.0.0.1:14222")

	nbus, err := natsbus.Connect(natsURL)
	if err != nil {
		log.Error("nats connect failed", "err", err)
		os.Exit(1)
	}

	m, err := manifest.ParseManifest(manifestYaml)
	if err != nil {
		log.Error("load manifest failed", "err", err)
		os.Exit(1)
	}

	if err := extension.Serve(
		extension.New(nbus, m.BuildConfig(manifest.Bindings{
			Handlers: s.handlers(),
			Variables: map[string]extension.VariableSpec{
				"org":      {Resolve: s.resolveOrg},
				"repo":     {Resolve: s.resolveRepo},
				"bookmark": {Resolve: s.resolveBookmark},
			},
			OnCallHook: s.onCallHook,
			OnLifecycle: func(ctx context.Context, ev abcprotocol.LifecycleEvent) error {
				return s.handleLifecycleEvent(ctx, string(ev.Kind), ev)
			},
		})),
		extension.ServeOptions{
			Handler: s.router(),
			Run: func(runCtx context.Context, ext *extension.Extension) {
				s.ext = ext
				log.Info("listening", "port", envOr("PORT", "8080"), "nats", natsURL)
				go runReconciler(runCtx, s, time.Duration(envInt("RECONCILE_INTERVAL_SECS", 60))*time.Second)
			},
		},
	); err != nil {
		log.Error("serve failed", "err", err)
		os.Exit(1)
	}
}

func pgConfig() PgConfig {
	name := envOr("REPOEXT_DB", "easylab_repoext.db")
	if !filepath.IsAbs(name) {
		name = filepath.Join(envOr("EASYVCS_HOME", homeDir()), name)
	}
	return PgConfig{DB: name}
}

// homeDir returns the easylab home (default ~/.easyvcs) for a local repoext db.
func homeDir() string {
	if h := os.Getenv("EASYVCS_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".easyvcs")
}
