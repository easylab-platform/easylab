package opsext

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var _ = fmt.Sprintf

// Built-in containerfile templates (phase 2, mirror original misc.rs).
func builtinTemplates() []map[string]string {
	return []map[string]string{
		{"name": "oci", "content": "FROM scratch\nCOPY . /app\n"},
		{"name": "cargo", "content": "FROM rust:1.97-alpine AS build\nWORKDIR /app\nCOPY . .\nRUN cargo build --release\nFROM alpine:3.20\nCOPY --from=build /app/target/release/app /app\n"},
		{"name": "npm", "content": "FROM node:22-alpine\nWORKDIR /app\nCOPY . .\nRUN npm install\nCMD [\"node\", \"index.js\"]\n"},
	}
}

func (s *server) containerfileTemplates(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{"templates": builtinTemplates()})
}

type buildBody struct {
	Org    string `json:"org"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Tag    string `json:"tag"`
	// ImageTag overrides the image reference tag. It decouples the source
	// revision (branch) from the image tag so releases can pin immutable
	// semver tags (e.g. v0.0.1) instead of the floating :dev. When empty the
	// historical behavior of tagging by branch is preserved.
	ImageTag   string   `json:"image-tag"`
	Dockerfile string   `json:"dockerfile"`
	Push       bool     `json:"push"`
	Raw        bool     `json:"raw"`
	NoCache    bool     `json:"no-cache"`
	BuildArgs  []string `json:"build_args"`
}

// ImageRefTag returns the image tag to append to the reference, preferring an
// explicit ImageTag over the branch/floating default.
func (b buildBody) ImageRefTag() string {
	if b.ImageTag != "" {
		return b.ImageTag
	}
	return b.BranchOrDefault()
}

// ForceNoCache resolves the effective no-cache flag. The raw-content build
// path has no content-addressable context (the Dockerfile is the whole
// context), so it must always invalidate — otherwise buildkit would serve the
// previous build's output forever even when the raw content changed.
func (b buildBody) ForceNoCache() bool {
	return b.NoCache || b.Raw
}

// BranchOrDefault returns the branch or "latest" for raw builds (needed
// to form a valid image reference without a repo).
func (b buildBody) BranchOrDefault() string {
	if b.Branch == "" {
		return "latest"
	}
	return b.Branch
}

func (s *server) buildImage(w http.ResponseWriter, r *http.Request) {
	var b buildBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}

	if !b.Raw {
		if b.Org == "" || b.Repo == "" || b.Branch == "" {
			writeErr(w, http.StatusBadRequest, "org/repo/branch required")
			return
		}
	}
	if b.Tag == "" {
		b.Tag = "latest"
	}
	if b.Raw && strings.TrimSpace(b.Dockerfile) == "" {
		writeErr(w, http.StatusBadRequest, "raw build requires dockerfile content")
		return
	}

	// Forward to easylab (it owns buildkitd, the repo store and the export).
	fullImage := s.artifactImageHost + "/" + b.Tag + ":" + b.ImageRefTag()
	req := map[string]interface{}{
		"org":      b.Org,
		"repo":     b.Repo,
		"branch":   b.Branch,
		"raw":      b.Raw,
		"image":    fullImage,
		"export":   "push",
		"no-cache": b.ForceNoCache(),
	}
	if b.Raw {
		req["containerfile"] = b.Dockerfile
	} else if b.Dockerfile != "" {
		req["dockerfile"] = b.Dockerfile
	}
	if len(b.BuildArgs) > 0 {
		req["build_args"] = b.BuildArgs
	}
	id, err := s.opsSubmitBuild(r.Context(), req)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	// Mirror the easylab task into the local registry so /builds/{id} (status +
	// SSE log) keeps working unchanged; the poller folds easylab state in.
	s.mirrorOpsTask(id, "build", b.Tag, fullImage)
	writeJSON(w, http.StatusAccepted, map[string]interface{}{"ok": true, "build_id": id})
}

// mirrorOpsTask registers a local task that tracks a easylab ops task until it
// leaves "running", copying status/result/error into the local view.
func (s *server) mirrorOpsTask(id, kind, tag, image string) {
	t := &buildTask{
		ID:        id,
		Kind:      kind,
		Tag:       tag,
		State:     "running",
		Image:     image,
		StartedAt: time.Now(),
		subs:      map[chan buildLogLine]struct{}{},
	}
	s.builds.Store(id, t)
	s.evictBuilds()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		res, err := s.opsTask(ctx, id)
		if err != nil {
			t.append(buildLogLine{Stream: kind, Line: "ERROR: " + err.Error()})
			t.setResult(image, err.Error())
			return
		}
		if res["status"] == "done" {
			if r, _ := res["result"].(string); r != "" {
				t.append(buildLogLine{Stream: kind, Line: r})
			}
			t.setResult(image, "")
			return
		}
		msg, _ := res["error"].(string)
		if msg == "" {
			msg = "easylab task " + res["status"].(string)
		}
		t.append(buildLogLine{Stream: kind, Line: "ERROR: " + msg})
		t.setResult(image, msg)
	}()
}
