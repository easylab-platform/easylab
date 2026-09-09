package repoext

import (
	"context"
	_ "embed"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
	"github.com/abcp-sdk/abc-protocol-go/extension"
	"github.com/abcp-sdk/abc-protocol-go/manifest"
	natsbus "github.com/abcp-sdk/abc-protocol-go/transport/nats"

	easylabsdk "github.com/easylab-platform/easylab-sdk-go"
)

//go:embed manifest.yaml
var manifestYaml []byte

type server struct {
	base  string // easylab base URL
	agent string // agent-ts base URL
	store *Store
	cache *sessCache
	lab   *easylabClient
	sdk   *easylabsdk.Client // typed easylab client (search/graph/compare/rebase/tree/revisions)
	ag    *agentClient
	ext   *extension.Extension
	bus   *natsbus.Bus
}

// Options configures the embedded repo-extension.
type Options struct {
	Base              string
	Agent             string
	NATSURL           string
	Token             string
	DB                string
	ReconcileInterval time.Duration
	Hook              func(ext *extension.Extension)
}

// Run starts the embedded repo-extension: registers the NATS tool face, the
// lifecycle-event subscription (mapping session<->branch), and the
// reconciler. No HTTP listener — served in-process by easylab (single
// binary). It registers subscriptions and returns.
func Run(ctx context.Context, opts Options) error {
	log := slog.Default().With("svc", "repo-extension")

	base := opts.Base
	if base == "" {
		base = envOr("EASYLAB_URL", "http://127.0.0.1:18160")
	}
	agent := opts.Agent
	if agent == "" {
		agent = envOr("AGENT_URL", "http://abcp-agent.temp.svc.cluster.local")
	}
	natsURL := opts.NATSURL
	if natsURL == "" {
		natsURL = envOr("NATS_URL", "nats://127.0.0.1:14222")
	}
	token := opts.Token
	if token == "" {
		token = envOr("EASYLAB_TOKEN", "devtoken")
	}

	s := &server{base: base, agent: agent}
	s.lab = newClient(base, token)
	s.sdk = easylabsdk.New(base, token)
	s.ag = newAgentClient(agent)
	s.cache = newSessCache(5 * time.Second)

	db := opts.DB
	if db == "" {
		db = envOr("REPOEXT_DB", "easylab_repoext.db")
	}
	if !filepath.IsAbs(db) {
		db = filepath.Join(envOr("EASYVCS_HOME", homeDir()), db)
	}
	store, err := OpenStore(ctx, PgConfig{DB: db})
	if err != nil {
		return err
	}
	s.store = store
	// The store lives for the process lifetime: Run registers subscriptions
	// and returns (embedded mode), so a deferred Close here would slam the DB
	// shut right after startup — every query would then fail with
	// "sql: database is closed". The process exit cleans up.

	nbus, err := natsbus.Connect(natsURL)
	if err != nil {
		return err
	}
	s.bus = nbus

	m, err := manifest.ParseManifest(manifestYaml)
	if err != nil {
		return err
	}

	ext := extension.New(nbus, m.BuildConfig(manifest.Bindings{
		Handlers: s.handlers(),
		Variables: map[string]extension.VariableSpec{
			"org":      {Resolve: s.resolveOrg},
			"repo":     {Resolve: s.resolveRepo},
			"branch": {Resolve: s.resolveBranch},
		},
		OnLifecycle: func(ctx context.Context, ev abcprotocol.LifecycleEvent) error {
			return s.handleLifecycleEvent(ctx, string(ev.Kind), ev)
		},
	}))

	if err := ext.Serve(ctx); err != nil {
		return err
	}
	s.ext = ext
	log.Info("repo-extension embedded, nats", "nats", natsURL, "easylab", base)
	interval := opts.ReconcileInterval
	if interval <= 0 {
		interval = time.Duration(envInt("RECONCILE_INTERVAL_SECS", 60)) * time.Second
	}
	go runReconciler(ctx, s, interval)
	if opts.Hook != nil {
		opts.Hook(ext)
	}
	return nil
}

// homeDir returns the easylab home (default ~/.easyvcs) for a local repoext db.
func homeDir() string {
	if h := os.Getenv("EASYVCS_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".easyvcs")
}
