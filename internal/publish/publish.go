// Package publish carries the per-protocol publish templates (containerfile
// bodies) and the renderer. It is the platform's built-in publish toolchain:
// a publish-protocol produce action turns protocol + args into a containerfile
// that easylab's oci-build/build backend runs (which uploads the package to
// the easylab protocol registry).
package publish

import (
	"strings"
)

// publishSpec is one protocol's publish template.
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
		// --registry name, so the env key must be uppercase (EASYLAB registry alias).
		image: "rust:1-alpine",
		steps: `RUN export CARGO_REGISTRIES_EASYLAB_INDEX="sparse+$ARTIFACT_URL/pkgs/cargo/index/" \
 && export CARGO_REGISTRIES_EASYLAB_TOKEN="${ARTIFACT_TOKEN:-dummy}" \
 && cargo publish --registry easylab --allow-dirty \
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
 && conan remote add easylab "$ARTIFACT_URL/pkgs/conan" --force \
 && { [ -z "$ARTIFACT_TOKEN" ] || conan remote login easylab agent -p "$ARTIFACT_TOKEN"; } \
 && conan create . \
 && conan upload '*' -r easylab -c \
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

// Protocol returns whether a protocol is supported.
func Protocol(name string) bool {
	_, ok := publishSpecs[name]
	return ok
}

// Supported lists the supported publish protocols.
func Supported() []string {
	out := make([]string, 0, len(publishSpecs))
	for p := range publishSpecs {
		out = append(out, p)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Required returns the mandatory args for a protocol.
func Required(protocol string) []string {
	spec, ok := publishSpecs[protocol]
	if !ok {
		return nil
	}
	return spec.required
}

// Render produces the containerfile for a protocol (used by the
// publish-protocol produce action). baseURL/token are the build-arg values.
func Render(protocol string, name, version, file, baseURL, token string) (string, error) {
	spec, ok := publishSpecs[protocol]
	if !ok {
		return "", ErrProtocol{protocol}
	}
	args := map[string]string{"NAME": name, "VERSION": version, "FILE": file}
	for _, req := range spec.required {
		if args[req] == "" {
			return "", errArgs{req, protocol}
		}
	}
	containerfile := renderPublishContainerfile(spec)
	// Return the rendered containerfile; build-arg values are passed to the
	// build backend separately (BuildArgs), so the render keeps ARG decls.
	return containerfile, nil
}

type ErrProtocol struct{ Name string }

func (e ErrProtocol) Error() string { return "unsupported protocol " + e.Name }

type errArgs struct {
	Arg      string
	Protocol string
}

func (e errArgs) Error() string { return "publish " + e.Protocol + " requires " + e.Arg }
