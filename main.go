package opsext

import (
	"context"
	_ "embed"
	"log/slog"
	"sync"

	abcprotocol "github.com/abcp-sdk/abc-protocol-go"
	"github.com/abcp-sdk/abc-protocol-go/extension"
	"github.com/abcp-sdk/abc-protocol-go/manifest"
	natsbus "github.com/abcp-sdk/abc-protocol-go/transport/nats"

	easylabsdk "github.com/easylab-platform/easylab-sdk-go"
)


//go:embed manifest.yaml
var manifestYaml []byte

type server struct {
	sdk               *easylabsdk.Client // typed easylab client (lab+ops+registry, owns all k8s access)
	bus               *natsbus.Bus       // NATS bus (file store / abc)
	runtimeNamespace  string             // namespace where easylab creates sandboxes/deployments
	ext               *extension.Extension
	artifact          string // artifact registry base URL (packages + OCI + metadata)
	artifactImageHost string // TLS ingress host for image refs (FROM/push via buildkit)
	artifactToken     string // optional bearer/basic token for artifact write auth
	base              string // easylab URL (repo archive + contents + clone)
	easylabToken      string // easylab write token (Authorization: token <…>)

	wsMu    sync.Mutex              // guards wsCache
	wsCache map[string]wsCacheEntry // session -> workspace (short TTL)

	builds sync.Map // build id -> *buildTask
}

// Options configures the embedded ops-extension.
type Options struct {
	EasyLabURL    string
	NATSURL       string
	Token         string
	ArtifactURL   string
	ArtifactHost  string
	ArtifactToken string
	RuntimeNS     string
	Port          string
	Hook          func(ext *extension.Extension)
}

// Run starts the embedded ops-extension: registers the NATS tool face (and
// lifecycle hook) so the agent can discover and call sandbox/build/deploy/
// package tools. No HTTP listener — it is served in-process by easylab (the
// single binary). It registers subscriptions and returns; the NATS
// subscriptions stay live for the process lifetime (ctx driven).
func Run(ctx context.Context, opts Options) error {
	log := slog.Default().With("svc", "ops-extension")

	base := opts.EasyLabURL
	if base == "" {
		base = envOr("EASYLAB_URL", "http://easylab:80")
	}
	natsURL := opts.NATSURL
	if natsURL == "" {
		natsURL = envOr("NATS_URL", "nats://nats.easylab.svc.cluster.local:4222")
	}
	artifact := opts.ArtifactURL
	if artifact == "" {
		artifact = trimTrailingSlash(envOr("ARTIFACT_URL", "http://easylab"))
	}
	artifactHost := opts.ArtifactHost
	if artifactHost == "" {
		artifactHost = envOr("ARTIFACT_IMAGE_HOST", "easylab")
	}
	artifactToken := opts.ArtifactToken
	if artifactToken == "" {
		artifactToken = envOr("ARTIFACT_TOKEN", "")
	}
	easylabToken := opts.Token
	if easylabToken == "" {
		easylabToken = envOr("EASYLAB_TOKEN", "devtoken")
	}
	ns := opts.RuntimeNS
	if ns == "" {
		ns = envOr("NAMESPACE", "easylab")
	}

	s := &server{
		sdk:               easylabsdk.New(base, easylabToken),
		artifact:          artifact,
		artifactImageHost: artifactHost,
		artifactToken:     artifactToken,
		base:              base,
		easylabToken:      easylabToken,
		runtimeNamespace:  ns,
		wsCache:           map[string]wsCacheEntry{},
	}

	nbus, err := natsbus.Connect(natsURL)
	if err != nil {
		return err
	}
	m, err := manifest.ParseManifest(manifestYaml)
	if err != nil {
		return err
	}
	s.bus = nbus

	ext := extension.New(nbus, m.BuildConfig(manifest.Bindings{
		Handlers: s.handlers(),
		Variables: map[string]extension.VariableSpec{
			"sandbox-id":     {Resolve: s.resolveSandboxID},
			"sandbox-status": {Resolve: s.resolveSandboxStatus},
		},
		OnLifecycle: func(ctx context.Context, ev abcprotocol.LifecycleEvent) error {
			if ev.Kind == "deleted" {
				s.clearSandboxVars(ctx, ev.SessionName)
			}
			return nil
		},
	}))

	// Register the extension's NATS subscriptions (tools/variables/hooks/
	// lifecycle) without serving an HTTP surface.
	if err := ext.Serve(ctx); err != nil {
		return err
	}
	s.ext = ext
	log.Info("ops-extension embedded, nats", "nats", natsURL, "easylab", base, "runtime-ns", ns)
	if opts.Hook != nil {
		opts.Hook(ext)
	}
	return nil
}
