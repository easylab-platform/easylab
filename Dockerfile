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
# Build context: this module (github.com/easylab-platform/easylab) only. The
# easyvcs engine is a separate public module (github.com/easylab-platform/easyvcs)
# resolved by GOPROXY - the replace directive in go.mod pins it for local dev
# but MUST be stripped for the container build so it resolves from the proxy.
# All Go deps (easyvcs, artifact/*, easylab-proto, abcp-sdk/*) come from public
# sources during the build.
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
    GOWORK=off \
    CGO_ENABLED=0
RUN apk add --no-cache ca-certificates
WORKDIR /src/app
COPY go.mod go.sum ./
COPY cmd ./cmd
COPY internal ./internal
# No vendor/: all deps (incl. easylab-proto, easylab-sdk-go, easyvcs) resolve
# from the module proxy; the build runs -mod=mod over the network.
RUN go build -mod=mod -trimpath -ldflags="-s -w" -o /out/easylab ./cmd/easylab

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
# easyworker binary injected into sandbox base images (derived-image builds).
COPY worker-bin/easyworker /usr/local/lib/easyworker/easyworker-linux-amd64
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
