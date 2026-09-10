#!/usr/bin/env bash
# Build and push the EasyLab runtime image WITHOUT a local build daemon.
#
# Pipeline (verified):
#   1. Assemble a temp build context rooted at '.'. The Go module graph
#      resolves EVERYTHING from public GitHub (easylab-platform/*, abcp-sdk/*)
#      via GOPROXY — no vendor/ directory and no local paths.
#   2. buildctl targets the shared cluster buildkitd (default the temp one) and
#      builds the image, exporting a docker archive (with a RepoTag). buildkitd
#      does NOT push.
#   3. skopeo copies the docker archive to forgejo OCI registry (the only
#      registry whose write path is authenticated & reachable from here).
#
# Prereqs: a reachable buildkitd, skopeo, and base images in forgejo OCI.
set -euo pipefail
LAB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

REGISTRY="${REGISTRY:-forgejo.develop.10.199.64.20.nip.io}"
NAMESPACE="${NAMESPACE:-easylab}"
NAME="${NAME:-easylab}"
TAG="${TAG:-$(date +%Y%m%d%H%M%S)}"
DEST="${REGISTRY}/${NAMESPACE}/${NAME}:${TAG}"
BUILDKIT="${BUILDKIT_ADDR:-tcp://buildkitd.temp.svc.cluster.local:1234}"
FORGEJO_USER="${FORGEJO_USER:-root}"
FORGEJO_PASS="${FORGEJO_PASS:-devpassword}"
PROXY="${PROXY:-http://mihomo.develop.svc.cluster.local:7890}"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# Build context root: the easylab module itself (go.mod + cmd/ + internal/).
# All deps resolve from GOPROXY (no replace directives, no vendor/); the build
# is -mod=mod over the network through the proxy.
CTX="${WORK}/ctx"
mkdir -p "${CTX}"
cp "${LAB_DIR}/go.mod" "${CTX}/go.mod"
cp "${LAB_DIR}/go.sum" "${CTX}/go.sum"
cp -r "${LAB_DIR}/cmd" "${CTX}/cmd"
cp -r "${LAB_DIR}/internal" "${CTX}/internal"
# easyworker binary injected into sandbox base images (EnsureSandboxImage).
# Prefer the sibling source tree's freshly built binary; otherwise pull the
# canonical one from the artifact registry (generic store), so a build host
# with neither the easyworker repo nor the binary still works.
mkdir -p "${CTX}/worker-bin"
WORKER_LOCAL="${WORKER_BIN_SRC:-${LAB_DIR}/../easyworker/dist/easyworker-linux-amd64}"
if [ -f "${WORKER_LOCAL}" ]; then
  cp "${WORKER_LOCAL}" "${CTX}/worker-bin/easyworker"
else
  WORKER_URL="${EASYLAB_ARTIFACT_URL:-http://easylab.temp.svc.cluster.local}/pkgs/generic/${WORKER_NAME:-easyworker}/${WORKER_VERSION:-v0.3.0}/easyworker-linux-amd64"
  echo "easyworker binary not at ${WORKER_LOCAL}; fetching ${WORKER_URL}"
  curl -fsSL ${ARTIFACT_TOKEN:+-H "Authorization: Bearer ${ARTIFACT_TOKEN}"} -o "${CTX}/worker-bin/easyworker" "${WORKER_URL}"
fi
mkdir -p "${CTX}/easy-lab" && cp "${LAB_DIR}/Dockerfile" "${CTX}/easy-lab/Dockerfile"

echo "Building EasyLab image -> ${DEST} (buildkitd=${BUILDKIT})"
buildctl --addr "${BUILDKIT}" build \
  --frontend dockerfile.v0 \
  --local "context=${CTX}" \
  --local "dockerfile=${CTX}" \
  --opt "filename=easy-lab/Dockerfile" \
  --opt "build-arg:HTTP_PROXY=${PROXY}" \
  --opt "build-arg:HTTPS_PROXY=${PROXY}" \
  --opt "build-arg:NO_PROXY=localhost,127.0.0.1,.svc.cluster.local,.svc,.nip.io,10.199.64.20,develop.10.199.64.20.nip.io" \
  --output "type=docker,name=${NAMESPACE}/${NAME}:${TAG},dest=${WORK}/image.tar" \
  --progress plain

echo "Pushing to forgejo ${DEST}"
skopeo copy \
  --dest-creds "${FORGEJO_USER}:${FORGEJO_PASS}" \
  --dest-tls-verify=false \
  "docker-archive:${WORK}/image.tar:${NAMESPACE}/${NAME}:${TAG}" \
  "docker://${DEST}"

echo "Verifying push:"
skopeo inspect --creds "${FORGEJO_USER}:${FORGEJO_PASS}" --tls-verify=false "docker://${DEST}" >/dev/null 2>&1 \
  && echo "OK ${DEST}" \
  || echo "inspect failed for ${DEST} (image may still be present)"
