# syntax=docker/dockerfile:1
# EasyLab: self-contained code-hosting + package/OCI registry + dev/deploy
# platform.
#
# Built WITH the cluster buildkitd: run ./build-image.sh (targets the shared
# buildkitd, then pushes to forgejo via skopeo).
#
# EasyLab now drives the podman privileged sidecar through the Docker-compatible
# API (docker/client, static, no CGO). It does NOT embed buildah/podman and does
# NOT need podman's container tooling — those live in the sidecar container.
# git is kept (mirror push). kubectl/helm are no longer needed (no k8s CLI
# interaction; service-deploy goes through the ops sidecar).
#
# Build context: go.work + easyvcs/ only. All Go deps (artifact/*, easylab-proto,
# abcp-sdk/*) resolve from public sources during the build.
ARG REGISTRY=forgejo.develop.10.199.64.20.nip.io/root
ARG ALPINE=3.24

# ---- easylab + easyvcs (static Go binaries) ----
FROM ${REGISTRY}/golang:1.26-alpine AS gobuild
ARG HTTP_PROXY=http://mihomo.develop.svc.cluster.local:7890
ARG HTTPS_PROXY=http://mihomo.develop.svc.cluster.local:7890
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    NO_PROXY=localhost,127.0.0.1,.svc.cluster.local,.svc,.nip.io,10.199.64.20,.develop.10.199.64.20.nip.io \
    GOPROXY=https://proxy.golang.org \
    GONOSUMDB=github.com/easylab-platform/*,github.com/abcp-sdk/* \
    CGO_ENABLED=0
RUN apk add --no-cache ca-certificates
WORKDIR /src
COPY go.work go.work.sum ./
COPY easyvcs ./easyvcs
RUN cd easyvcs && go build -trimpath -ldflags="-s -w" -o /out/easylab ./cmd/easylab \
    && go build -trimpath -ldflags="-s -w" -o /out/easyvcs ./cmd/easyvcs

# ---- runtime ----
FROM ${REGISTRY}/alpine:${ALPINE}
ARG HTTP_PROXY=http://mihomo.develop.svc.cluster.local:7890
ARG HTTPS_PROXY=http://mihomo.develop.svc.cluster.local:7890
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    NO_PROXY=localhost,127.0.0.1,mirrors.aliyun.com,.svc.cluster.local,.svc,.nip.io,10.199.64.20,.develop.10.199.64.20.nip.io
RUN set -eux; \
    sed -i 's|dl-cdn.alpinelinux.org|mirrors.aliyun.com|g' /etc/apk/repositories; \
    apk add --no-cache \
        ca-certificates git curl \
    && addgroup -S easyvcs && adduser -S -G easyvcs easyvcs
# /data is writable (hostPath) and holds the persistent OCI metadata + blobs.
COPY --from=gobuild /out/easylab /usr/local/bin/easylab
COPY --from=gobuild /out/easyvcs /usr/local/bin/easyvcs
RUN cat > /usr/local/bin/easylab-entrypoint.sh <<'EOF'
#!/bin/sh
exec /usr/local/bin/easylab "$@"
EOF
RUN chmod +x /usr/local/bin/easylab-entrypoint.sh
ENV EASYVCS_HOME=/data \
    EASYVCS_SELF_BASE="" \
    EASYVCS_OPS_NAMESPACES=default \
    EASYVCS_BUILD_BACKEND=podman \
    EASYLAB_PODMAN_URI=unix:///run/podman/podman.sock \
    DOCKER_HOST=unix:///run/podman/podman.sock
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/easylab-entrypoint.sh"]
CMD ["-addr", "0.0.0.0:8080"]
