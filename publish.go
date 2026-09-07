// Containerfile-based package publishing.
//
// Instead of hand-writing HTTP clients for every registry protocol, each
// protocol gets a containerfile template that runs the official CLI (or a
// minimal uploader) inside a buildkit build with no image export — only the
// RUN side effects (the publish) matter. The repo checkout from easylab is
// the build context.
//
// Base images are plain docker.io references: buildkitd resolves them through
// its own proxy config, and layer cache is shared on the buildkitd instance.
// Uploads go to $ARTIFACT_URL (the in-cluster artifact service, plain HTTP).
//
// Cache-busting: buildkit caches RUN layers by command+context. Every publish
// step references $PUBLISH_TS (a fresh timestamp per invocation), so the
// publish layer never hits cache while toolchain layers stay cached.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// publishSpec describes one protocol's publish pipeline.
type publishSpec struct {
	// image is the base image reference (plain docker.io name, e.g. "node:22-alpine").
	// Empty when the template is multi-stage and carries its own FROMs.
	image string
	// args are extra build-args the template uses (NAME/VERSION/FILE).
	args []string
	// required args that must be non-empty before building.
	required []string
	// steps is the containerfile body after WORKDIR/COPY (single-stage), or
	// the entire body after the ARG preamble when multi is true.
	steps string
	multi bool
}

// Standard build-args every template receives.
const (
	argArtifactURL = "ARTIFACT_URL" // full base URL, used in RUN commands
	argArtifactTok = "ARTIFACT_TOKEN"
	argPublishTS   = "PUBLISH_TS"
)

var publishSpecs = map[string]publishSpec{
	"npm": {
		// npm ≥9 refuses to publish without client-side credentials even
		// against anonymous registries; a project .npmrc with a (possibly
		// dummy) token satisfies it. The key must be the normalized URL
		// (default port stripped) or npm won't match it.
		image: "node:22-alpine",
		args:  []string{"NPMRC_LINE"},
		steps: `RUN echo "$NPMRC_LINE" > .npmrc \
 && npm publish --registry "$ARTIFACT_URL/pkgs/npm/" \
 && echo "$PUBLISH_TS" > /dev/null`,
	},
	"pypi": {
		// Toolchain and upload both go through the artifact pypi proxy (single
		// egress). trusted-host is required because the in-cluster base URL is
		// plain HTTP.
		image: "python:3.12-alpine",
		steps: `RUN pip config set global.index-url "$ARTIFACT_URL/pkgs/pypi/simple" \
 && pip config set global.trusted-host "$(echo "$ARTIFACT_URL" | sed 's|.*://||; s|[:/].*||')" \
 && pip install --no-cache-dir build twine \
 && { [ -d dist ] && [ -n "$(ls -A dist 2>/dev/null)" ] || python -m build; } \
 && twine upload --repository-url "$ARTIFACT_URL/pkgs/pypi/" -u agent -p "${ARTIFACT_TOKEN:-dummy}" --non-interactive dist/* \
 && echo "$PUBLISH_TS" > /dev/null`,
	},
	"cargo": {
		// Cargo matches CARGO_REGISTRIES_<NAME>_* env vars by uppercasing the
		// --registry name, so the env key must be uppercase (ZERGX).
		image: "rust:1-alpine",
		steps: `RUN export CARGO_REGISTRIES_ZERGX_INDEX="sparse+$ARTIFACT_URL/pkgs/cargo/index/" \
 && export CARGO_REGISTRIES_ZERGX_TOKEN="${ARTIFACT_TOKEN:-dummy}" \
 && cargo publish --registry zergx --allow-dirty \
 && echo "$PUBLISH_TS" > /dev/null`,
	},
	"rubygems": {
		image: "ruby:3.3-alpine",
		steps: `RUN gem build *.gemspec \
 && GEM="$(ls -t *.gem 2>/dev/null | head -n1)" \
 && [ -n "$GEM" ] || { echo 'no *.gem produced; check the gemspec'; exit 1; } \
 && mkdir -p "$HOME/.gem" \
 && printf ':rubygems_api_key: %s\n' "$ARTIFACT_TOKEN" > "$HOME/.gem/credentials" \
 && chmod 0600 "$HOME/.gem/credentials" \
 && gem push "$GEM" --host "$ARTIFACT_URL/pkgs/rubygems" \
 && echo "$PUBLISH_TS" > /dev/null`,
	},
	"helm": {
		// Two stages: helm packages the chart, curl uploads it (chart-museum API).
		multi: true,
		steps: `FROM alpine/helm:3.16 AS pkg
WORKDIR /pkg
COPY . .
RUN helm package .

FROM curlimages/curl:8.11.1
WORKDIR /pkg
ARG ARTIFACT_URL
ARG ARTIFACT_TOKEN
ARG PUBLISH_TS
COPY --from=pkg /pkg/*.tgz .
RUN curl -sSf -H "Authorization: Bearer $ARTIFACT_TOKEN" -F "chart=@$(ls *.tgz | head -n1)" "$ARTIFACT_URL/pkgs/helm/api/charts" \
 && echo "$PUBLISH_TS" > /dev/null`,
	},
	"nuget": {
		// Assumes the .nupkg was already built (e.g. in the sandbox and ported
		// into the repo); pushes it via the nuget push API.
		image: "curlimages/curl:8.11.1",
		steps: `RUN NUPKG="$(find . -name '*.nupkg' | head -n1)" \
 && [ -n "$NUPKG" ] || { echo 'no *.nupkg found; build the project first (dotnet pack) and commit/port the artifact'; exit 1; } \
 && curl -sSf -X PUT -H "X-NuGet-ApiKey: $ARTIFACT_TOKEN" --data-binary @"$NUPKG" "$ARTIFACT_URL/pkgs/nuget/v3/package" \
 && echo "$PUBLISH_TS" > /dev/null`,
	},
	"maven": {
		// Assumes the jar was already built; PUTs it at the proper coordinates:
		// /pkgs/maven/<groupId dots-as-slashes>/<version>/<file>.
		image:    "curlimages/curl:8.11.1",
		args:     []string{"NAME", "VERSION"},
		required: []string{"NAME", "VERSION"},
		steps: `RUN JAR="$(find . -name '*.jar' -not -path './.m2/*' | head -n1)" \
 && [ -n "$JAR" ] || { echo 'no *.jar found; build the project first (mvn package) and commit/port the artifact'; exit 1; } \
 && curl -sSf -X PUT -H "Authorization: Bearer $ARTIFACT_TOKEN" --data-binary @"$JAR" \
    "$ARTIFACT_URL/pkgs/maven/$(echo "$NAME" | tr '.' '/')/$VERSION/$(basename "$JAR")" \
 && echo "$PUBLISH_TS" > /dev/null`,
	},
	"go": {
		// Builds a proper GOPROXY module zip (module@version/ prefix) and PUTs
		// it to the artifact's go upload endpoint. Python stdlib only.
		image:    "library/python:3.12-alpine",
		args:     []string{"NAME", "VERSION"},
		required: []string{"NAME", "VERSION"},
		steps: `RUN PUBLISH_TS="$PUBLISH_TS" python <<'EOF'
import io, os, urllib.parse, urllib.request, zipfile
name, ver = os.environ["NAME"], os.environ["VERSION"]
base, tok = os.environ["ARTIFACT_URL"], os.environ["ARTIFACT_TOKEN"]
buf = io.BytesIO()
with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
    for root, dirs, files in os.walk("."):
        dirs[:] = [d for d in dirs if d not in (".git", "target", "node_modules")]
        for f in files:
            p = os.path.join(root, f)
            z.write(p, "%s@%s/%s" % (name, ver, os.path.relpath(p, ".")))
q = urllib.parse.urlencode({"name": name, "version": ver})
req = urllib.request.Request(base + "/pkgs/go/upload?" + q, data=buf.getvalue(), method="PUT")
if tok:
    req.add_header("Authorization", "Bearer " + tok)
print("go publish", name, ver, "->", urllib.request.urlopen(req).status, os.environ["PUBLISH_TS"])
EOF`,
	},
	"hex": {
		// Tars the project and POSTs it to the hex publish endpoint (the
		// artifact hex adapter stores the tar as-is).
		image:    "library/python:3.12-alpine",
		args:     []string{"NAME", "VERSION"},
		required: []string{"NAME", "VERSION"},
		steps: `RUN PUBLISH_TS="$PUBLISH_TS" python <<'EOF'
import io, os, tarfile, urllib.parse, urllib.request
name, ver = os.environ["NAME"], os.environ["VERSION"]
base, tok = os.environ["ARTIFACT_URL"], os.environ["ARTIFACT_TOKEN"]
buf = io.BytesIO()
with tarfile.open(fileobj=buf, mode="w:gz") as t:
    for root, dirs, files in os.walk("."):
        dirs[:] = [d for d in dirs if d not in (".git", "_build", "deps", "node_modules")]
        for f in files:
            p = os.path.join(root, f)
            t.add(p, arcname=os.path.relpath(p, "."))
q = urllib.parse.urlencode({"name": name, "version": ver})
req = urllib.request.Request(base + "/pkgs/hex/publish?" + q, data=buf.getvalue(), method="POST")
if tok:
    req.add_header("Authorization", "Bearer " + tok)
print("hex publish", name, ver, "->", urllib.request.urlopen(req).status, os.environ["PUBLISH_TS"])
EOF`,
	},
	"composer": {
		// Zips the project (composer.json included) and PUTs it to the
		// composer upload endpoint; the adapter extracts autoload metadata.
		image:    "library/python:3.12-alpine",
		args:     []string{"NAME", "VERSION"},
		required: []string{"NAME", "VERSION"},
		steps: `RUN PUBLISH_TS="$PUBLISH_TS" python <<'EOF'
import io, os, urllib.parse, urllib.request, zipfile
name, ver = os.environ["NAME"], os.environ["VERSION"]
base, tok = os.environ["ARTIFACT_URL"], os.environ["ARTIFACT_TOKEN"]
buf = io.BytesIO()
with zipfile.ZipFile(buf, "w", zipfile.ZIP_DEFLATED) as z:
    for root, dirs, files in os.walk("."):
        dirs[:] = [d for d in dirs if d not in (".git", "vendor", "node_modules")]
        for f in files:
            p = os.path.join(root, f)
            z.write(p, os.path.relpath(p, "."))
q = urllib.parse.urlencode({"name": name, "version": ver})
req = urllib.request.Request(base + "/pkgs/composer/api/packages?" + q, data=buf.getvalue(), method="PUT")
if tok:
    req.add_header("Authorization", "Bearer " + tok)
print("composer publish", name, ver, "->", urllib.request.urlopen(req).status, os.environ["PUBLISH_TS"])
EOF`,
	},
	"generic": {
		// Uploads one arbitrary file from the repo at /pkgs/generic/<name>/<version>/<file>.
		image:    "curlimages/curl:8.11.1",
		args:     []string{"NAME", "VERSION", "FILE"},
		required: []string{"NAME", "VERSION", "FILE"},
		steps: `RUN [ -f "$FILE" ] || { echo "file not found in repo: $FILE"; exit 1; } \
 && curl -sSf -X PUT -H "Authorization: Bearer $ARTIFACT_TOKEN" --data-binary @"$FILE" \
    "$ARTIFACT_URL/pkgs/generic/$NAME/$VERSION/$(basename "$FILE")" \
 && echo "$PUBLISH_TS" > /dev/null`,
	},
	"conan": {
		// conan 2 via pip (pypi proxied through artifact); create + upload.
		image: "python:3.12-alpine",
		steps: `RUN pip config set global.index-url "$ARTIFACT_URL/pkgs/pypi/simple" \
 && pip config set global.trusted-host "$(echo "$ARTIFACT_URL" | sed 's|.*://||; s|[:/].*||')" \
 && pip install --no-cache-dir 'conan>=2' \
 && conan profile detect --force \
 && conan remote add zergx "$ARTIFACT_URL/pkgs/conan" --force \
 && { [ -z "$ARTIFACT_TOKEN" ] || conan remote login zergx agent -p "$ARTIFACT_TOKEN"; } \
 && conan create . \
 && conan upload '*' -r zergx -c \
 && echo "$PUBLISH_TS" > /dev/null`,
	},
	"pub": {
		// Official dart CLI against the artifact pub API. dart's publisher
		// validation hard-requires a LICENSE file; synthesize one when the
		// repo has none.
		image: "dart:stable",
		steps: `RUN [ -f LICENSE ] || printf 'MIT License\n' > LICENSE \
 && { [ -z "$ARTIFACT_TOKEN" ] || dart pub token add "$ARTIFACT_URL/pkgs/pub" "$ARTIFACT_TOKEN"; } \
 && dart pub publish --force --server "$ARTIFACT_URL/pkgs/pub" \
 && echo "$PUBLISH_TS" > /dev/null`,
	},
	"swift": {
		// swift package archive-source produces the SE-0321 source zip; PUT it
		// at /pkgs/swift/<scope.name>/<version>. NAME must be "scope.pkgname".
		// (docker.io has no floating "swift:6" tag — pin a real one.)
		image:    "swift:6.1",
		args:     []string{"NAME", "VERSION"},
		required: []string{"NAME", "VERSION"},
		steps: `RUN if ! command -v zip >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1; then apt-get update -qq && apt-get install -y -qq zip curl; fi
RUN swift package archive-source --output /tmp/src.zip \
 && curl -sSf -X PUT -H "Authorization: Bearer $ARTIFACT_TOKEN" --data-binary @/tmp/src.zip \
    "$ARTIFACT_URL/pkgs/swift/$(echo "$NAME" | tr '.' '/')/$VERSION" \
 && echo "$PUBLISH_TS" > /dev/null`,
	},
}

// renderPublishContainerfile assembles the containerfile for a spec: ARG
// preamble, base stage, re-declared ARGs (usable in RUN), then the template
// body. Base images are plain docker.io references — buildkitd resolves them
// through its own proxy config; the artifact OCI pull-through currently does
// not fetch uncached tags on demand.
func renderPublishContainerfile(spec publishSpec) string {
	var b strings.Builder
	b.WriteString("ARG " + argArtifactURL + "\n")
	b.WriteString("ARG " + argArtifactTok + "\n")
	b.WriteString("ARG " + argPublishTS + "\n")
	for _, a := range spec.args {
		b.WriteString("ARG " + a + "\n")
	}
	if spec.multi {
		b.WriteString(spec.steps)
		b.WriteString("\n")
		return b.String()
	}
	b.WriteString("FROM " + spec.image + "\n")
	// Re-declare after FROM so the values reach RUN (an ARG re-declared without
	// a default inherits the build-arg value from before the first FROM).
	b.WriteString("ARG " + argArtifactURL + "\n")
	b.WriteString("ARG " + argArtifactTok + "\n")
	b.WriteString("ARG " + argPublishTS + "\n")
	for _, a := range spec.args {
		b.WriteString("ARG " + a + "\n")
	}
	b.WriteString("WORKDIR /pkg\n")
	b.WriteString("COPY . .\n")
	b.WriteString(spec.steps)
	b.WriteString("\n")
	return b.String()
}

// publishPackage publishes org/repo@branch as `protocol` via a buildkit-run
// containerfile. name/version/file map to the per-protocol build-args;
// dockerfilePath overrides the built-in template with a file from the repo.
func (s *server) publishPackage(ctx context.Context, protocol, org, repo, branch, name, version, file, dockerfilePath string) (string, error) {
	spec, ok := publishSpecs[protocol]
	if !ok {
		return "", fmt.Errorf("unsupported protocol %q; supported: %s", protocol, supportedProtocols())
	}
	if org == "" || repo == "" {
		return "", fmt.Errorf("package-publish: org/repo required (directly or via session context)")
	}
	if branch == "" {
		branch = "main"
	}

	buildArgs := map[string]string{
		"NAME":    name,
		"VERSION": version,
		"FILE":    file,
	}
	for _, req := range spec.required {
		if strings.TrimSpace(buildArgs[req]) == "" {
			return "", fmt.Errorf("package-publish: %s publish requires %q (got empty)", protocol, strings.ToLower(req))
		}
	}
	// Only pass per-protocol args the template actually declares.
	specArgs := map[string]string{}
	for _, a := range spec.args {
		specArgs[a] = buildArgs[a]
	}

	// 1. Render the containerfile (protocol template or one from the repo).
	// For a custom dockerfile_path we fetch the file via easylab contents API.
	containerfile := renderPublishContainerfile(spec)
	if dockerfilePath != "" {
		data, err := s.fetchRepoFile(ctx, org, repo, branch, dockerfilePath)
		if err != nil {
			return "", fmt.Errorf("read dockerfile %s: %w", dockerfilePath, err)
		}
		containerfile = string(data)
	}

	// 2. Standard + per-protocol build args (easylab passes them to buildkit).
	specArgs[argArtifactURL] = s.artifact
	specArgs[argArtifactTok] = s.artifactToken
	specArgs[argPublishTS] = strconv.FormatInt(time.Now().UnixNano(), 10)
	// npm: precomputed .npmrc line (token defaults to a dummy value — npm ≥9
	// requires client-side credentials even for anonymous registries, and the
	// key must be the normalized URL or getCredentialsByURI won't match it).
	if protocol == "npm" {
		token := s.artifactToken
		if token == "" {
			token = "anonymous"
		}
		specArgs["NPMRC_LINE"] = fmt.Sprintf("//%s/pkgs/npm/:_authToken=%s", hostOf(s.artifact), token)
	}
	argList := make([]string, 0, len(specArgs))
	for k, v := range specArgs {
		if v != "" {
			argList = append(argList, k+"="+v)
		}
	}
	sort.Strings(argList)

	// 3. Run through easylab's build (export=none) and await it.
	id, err := s.opsSubmitBuild(ctx, map[string]interface{}{
		"org":           org,
		"repo":          repo,
		"branch":        branch,
		"raw":           true,
		"containerfile": containerfile,
		"export":        "none",
		"build_args":    argList,
	})
	if err != nil {
		return "", fmt.Errorf("%s publish submit: %w", protocol, err)
	}
	res, err := s.opsTask(ctx, id)
	if err != nil {
		return "", fmt.Errorf("%s publish task: %w", protocol, err)
	}
	if res["status"] != "done" {
		msg, _ := res["error"].(string)
		if msg == "" {
			msg = "task " + res["status"].(string)
		}
		return "", fmt.Errorf("%s publish failed: %s", protocol, msg)
	}

	// 4. The RUN exiting 0 is the success signal (CLI failures fail the step).
	contextRef := fmt.Sprintf("%s/%s@%s", org, repo, branch)
	if name != "" && version != "" {
		return fmt.Sprintf("Published %s %s@%s (context %s).", protocol, name, version, contextRef), nil
	}
	return fmt.Sprintf("Published %s package (context %s).", protocol, contextRef), nil
}

func supportedProtocols() string {
	ps := make([]string, 0, len(publishSpecs))
	for p := range publishSpecs {
		ps = append(ps, p)
	}
	sort.Strings(ps)
	return strings.Join(ps, ", ")
}

// fetchRepoFile reads one file from a repo snapshot via easylab's contents API.
func (s *server) fetchRepoFile(ctx context.Context, org, repo, rev, path string) ([]byte, error) {
	u := fmt.Sprintf("%s/api/v1/repos/%s/%s/contents/%s?ref=%s",
		s.base, urlPathEscape(org), urlPathEscape(repo), escapePath(path), urlPathEscape(rev))
	body, err := s.httpGetRaw(ctx, u)
	if err != nil {
		return nil, err
	}
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("contents %s: bad response: %w", path, err)
	}
	if out.Encoding == "base64" {
		return base64.StdEncoding.DecodeString(out.Content)
	}
	return []byte(out.Content), nil
}
