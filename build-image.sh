#!/usr/bin/env bash
# Build and push the EasyLab runtime image WITHOUT a local build daemon.
#
# Pipeline (verified):
#   1. Assemble a temp build context rooted at '.', containing go.work +
#      easyvcs/. The Go module graph now resolves EVERYTHING from public
#      GitHub (easylab-platform/artifact/*, easylab-proto, abcp-sdk/*) — no
#      local pkr/ or deps/ needed. go.work only lists ./easyvcs.
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

# Build context root: holds go.work (./ path) + easyvcs/ + easy-lab/Dockerfile.
# easyvcs/go.mod requires github.com/easylab-platform/artifact/*,
# github.com/easylab-platform/easylab-proto, github.com/abcp-sdk/* — all public,
# resolved by network inside the buildkitd build. No local vendoring.
CTX="${WORK}/ctx"
mkdir -p "${CTX}"
if [ -d "${LAB_DIR}/easyvcs" ]; then
  cp -r "${LAB_DIR}/easyvcs" "${CTX}/easyvcs"
else
  cp -r "${LAB_DIR%/easy-lab}/easyvcs" "${CTX}/easyvcs"
fi
# The in-container go.work uses './easyvcs' only (context root holds easyvcs/).
cat > "${CTX}/go.work" <<'GOWORK'
go 1.26.5

use (
	./easyvcs
)
GOWORK
[ -f "${LAB_DIR}/go.work.sum" ] && cp "${LAB_DIR}/go.work.sum" "${CTX}/go.work.sum"
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
