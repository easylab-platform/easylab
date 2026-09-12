#!/usr/bin/env bash
# Build and push the easylab buildkit-worker image (buildkitd + easyworker in
# one container) WITHOUT a local build daemon: cluster buildkitd -> docker
# archive -> skopeo -> forgejo OCI. Mirrors build-image.sh.
set -euo pipefail
LAB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

REGISTRY="${REGISTRY:-forgejo.develop.10.199.64.20.nip.io}"
NAMESPACE="${NAMESPACE:-easylab}"
NAME="${NAME:-buildkit-worker}"
TAG="${TAG:-$(date +%Y%m%d%H%M%S)}"
DEST="${REGISTRY}/${NAMESPACE}/${NAME}:${TAG}"
BUILDKIT="${BUILDKIT_ADDR:-tcp://buildkitd.temp.svc.cluster.local:1234}"
FORGEJO_USER="${FORGEJO_USER:-root}"
FORGEJO_PASS="${FORGEJO_PASS:-devpassword}"
PROXY="${PROXY:-http://mihomo.develop.svc.cluster.local:7890}"
BASE_REGISTRY="${BASE_REGISTRY:-forgejo.develop.10.199.64.20.nip.io/root}"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

CTX="${WORK}/ctx"
mkdir -p "${CTX}/buildkit" "${CTX}/worker-bin"
cp "${LAB_DIR}/buildkit/Dockerfile" "${CTX}/buildkit/Dockerfile"
cp "${LAB_DIR}/buildkit/entrypoint.sh" "${CTX}/buildkit/entrypoint.sh"
WORKER_LOCAL="${WORKER_BIN_SRC:-${LAB_DIR}/../easyworker/dist/easyworker-linux-amd64}"
if [ ! -f "${WORKER_LOCAL}" ]; then
  WORKER_LOCAL="${LAB_DIR}/worker-bin/easyworker"
fi
cp "${WORKER_LOCAL}" "${CTX}/worker-bin/easyworker"

echo "Building ${NAME} -> ${DEST} (buildkitd=${BUILDKIT})"
buildctl --addr "${BUILDKIT}" build \
  --frontend dockerfile.v0 \
  --local "context=${CTX}" \
  --local "dockerfile=${CTX}/buildkit" \
  --opt "filename=Dockerfile" \
  --opt "build-arg:REGISTRY=${BASE_REGISTRY}" \
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

echo "${DEST}" > "${LAB_DIR}/.last-buildkit-image"
