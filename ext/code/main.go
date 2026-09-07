package main

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"

	"github.com/abcp-sdk/abc-protocol-go/extension"
	"github.com/abcp-sdk/abc-protocol-go/manifest"
	"github.com/abcp-sdk/abc-protocol-go/transport/nats"
)

//go:embed code.manifest.yaml
var manifestYaml []byte

func main() {
	log := slog.Default().With("svc", "easyvcs-code")

	natsURL := envOr("EASYVCS_NATS_URL", "nats://127.0.0.1:4222")
	base := envOr("EASYVCS_BASE", "http://127.0.0.1:18160")

	bus, err := nats.Connect(natsURL)
	if err != nil {
		log.Error("nats connect failed", "err", err)
		os.Exit(1)
	}
	m, err := manifest.ParseManifest(manifestYaml)
	if err != nil {
		log.Error("load manifest failed", "err", err)
		os.Exit(1)
	}

	c := newClient(base)

	cfg := m.BuildConfig(manifest.Bindings{
		Handlers: map[string]extension.ToolSpec{
			"list":          {Execute: c.list},
			"read":          {Execute: c.read},
			"write":         {Execute: c.write},
			"edit":          {Execute: c.edit},
			"delete":        {Execute: c.delete},
			"search":        {Execute: c.search},
			"revision_diff": {Execute: c.revisionDiff},
			"log":           {Execute: c.log},
			"blame":         {Execute: c.blame},
			"refs":          {Execute: c.refs},
			"history":       {Execute: c.history},
		},
	})

	if err := extension.Serve(extension.New(bus, cfg), extension.ServeOptions{
		Run: func(ctx context.Context, _ *extension.Extension) {
			log.Info("code extension started", "id", m.ID, "nats", natsURL, "base", base)
		},
	}); err != nil {
		log.Error("serve failed", "err", err)
		os.Exit(1)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// toolResult is a convenience constructor (single value; callers add , nil).
func toolResult(content string, data map[string]any) extension.ToolResultData {
	return extension.ToolResultData{Content: content, Data: data}
}

var _ = fmt.Sprintf
