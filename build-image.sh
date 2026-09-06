#!/usr/bin/env bash
# Build and push the EasyLab runtime image WITHOUT a local build daemon.
#
# Pipeline (verified):
#   1. Assemble a temp build context rooted at '.', containing go.work +
#      easyvcs/ + pkr/ so the Go module graph resolves. The go.work uses './'
#      paths (context-root relative) so the in-container build resolves.
#   2. buildctl targets the shared cluster buildkitd (default the temp one) and
#      builds the image, exporting a docker archive (with a RepoTag). buildkitd
#      does NOT push.
#   3. skopeo copies the docker archive to forgejo OCI registry (the only
#      registry whose write path is authenticated & reachable from here; its
#      token exchange rejects buildkitd, but skopeo's basic->token works).
#
# The resulting EasyLab image embeds buildah + podman, made self-contained/
# rootless-ready for RUNNING user image builds and service/sandbox containers
# inside the pod.
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

# Build context root: holds go.work (./ paths), easyvcs/, pkr/, and
# easy-lab/Dockerfile. easyvcs/go.mod has `replace github.com/pkr/pkrkit =>
# ../../pkr/pkrkit` which resolves (from /src/easyvcs) to /src/pkr/pkrkit — the
# same dir the go.work also uses, so no conflicting replace remains.
CTX="${WORK}/ctx"
mkdir -p "${CTX}"
if [ -d "${LAB_DIR}/easyvcs" ]; then
  cp -r "${LAB_DIR}/easyvcs" "${CTX}/easyvcs"
else
  cp -r "${LAB_DIR%/easy-lab}/easyvcs" "${CTX}/easyvcs"
fi
# The in-container go.work uses ./pkr/pkrkit as a workspace module, so the
# module-local `replace github.com/pkr/pkrkit => ../../pkr/pkrkit` in
# easyvcs/go.mod would conflict (Go rejects a module that is both a workspace
# member and a replace target). Drop it; the workspace resolves pkrkit.
sed -i '/^replace github.com\/pkr\/pkrkit => ..\/..\/pkr\/pkrkit/d' "${CTX}/easyvcs/go.mod"
cp -r "${LAB_DIR%/easy-lab}/pkr" "${CTX}/pkr"
# The in-container go.work must use './' paths (context root holds easyvcs/
# and pkr/ side by side), not the host '../' layout.
cat > "${CTX}/go.work" <<'GOWORK'
go 1.26.5

use (
	./easyvcs
	./pkr/pkr-cargo
	./pkr/pkr-composer
	./pkr/pkr-conan
	./pkr/pkr-generic
	./pkr/pkr-go
	./pkr/pkr-helm
	./pkr/pkr-hex
	./pkr/pkr-maven
	./pkr/pkr-npm
	./pkr/pkr-nuget
	./pkr/pkr-oci
	./pkr/pkr-pub
	./pkr/pkr-pypi
	./pkr/pkr-rubygems
	./pkr/pkr-swift
	./pkr/pkr-system
	./pkr/pkrkit
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
