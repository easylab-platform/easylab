# syntax=docker/dockerfile:1
# EasyLab: self-contained code-hosting + package/OCI registry + dev/deploy
# platform.
#
# This image is built WITH the cluster buildkitd: run ./build-image.sh (which
# targets the shared buildkitd and then pushes to forgejo via skopeo). That is
# the ONLY way this image is produced; it is not built by a local daemon.
#
# Inside the image we embed buildah so EasyLab builds END-USER images at
# runtime, self-contained (rootless, daemonless) with the OCI registry cache.
# buildctl is NOT embedded — runtime image builds only ever use buildah.
#
# The build context is a temp workspace assembled by build-image.sh: it holds
# go.work (paths rooted at the context root) and easyvcs/. ALL Go dependencies
# (github.com/easylab-platform/artifact/*, easylab-proto, abcp-sdk/*) resolve
# from public GitHub during the build — no local pkr/ or deps/ vendored.
# Base images come from forgejo OCI (root/golang, root/alpine present there).
ARG REGISTRY=forgejo.develop.10.199.64.20.nip.io/root
ARG ALPINE=3.24

# ---- easylab + easyvcs (static Go binaries) ----
FROM ${REGISTRY}/golang:1.26-alpine AS gobuild
ARG HTTP_PROXY=http://mihomo.develop.svc.cluster.local:7890
ARG HTTPS_PROXY=http://mihomo.develop.svc.cluster.local:7890
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    NO_PROXY=localhost,127.0.0.1,.svc.cluster.local,.svc,.nip.io,10.199.64.20,.develop.10.199.64.20.nip.io \
    GOPROXY=direct \
    GOSUMDB=off
WORKDIR /src
COPY go.work go.work.sum ./
COPY easyvcs ./easyvcs
RUN cd easyvcs && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/easylab ./cmd/easylab \
    && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/easyvcs ./cmd/easyvcs

# ---- runtime ----
FROM ${REGISTRY}/alpine:${ALPINE}
ARG HTTP_PROXY=http://mihomo.develop.svc.cluster.local:7890
ARG HTTPS_PROXY=http://mihomo.develop.svc.cluster.local:7890
ENV HTTP_PROXY=${HTTP_PROXY} \
    HTTPS_PROXY=${HTTPS_PROXY} \
    NO_PROXY=localhost,127.0.0.1,mirrors.aliyun.com,.svc.cluster.local,.svc,.nip.io,10.199.64.20,.develop.10.199.64.20.nip.io
# Use the Aliyun apk mirror so package downloads are fast and reachable inside
# the cluster build; it is reachable directly (bypassing the proxy is safe via
# NO_PROXY) and via the egress proxy for any non-mirror hosts.
RUN set -eux; \
    sed -i 's|dl-cdn.alpinelinux.org|mirrors.aliyun.com|g' /etc/apk/repositories; \
    apk add --no-cache \
        ca-certificates git curl kubectl helm \
        buildah podman fuse-overlayfs netavark slirp4netns shadow-subids \
        iptables iproute2 aardvark-dns \
    && addgroup -S easyvcs && adduser -S -G easyvcs easyvcs
# give the easyvcs user a userns (subuid/subgid) range so buildah/podman can run
# rootless inside the pod.
RUN echo "easyvcs:200000:65536" >> /etc/subuid \
    && echo "easyvcs:200000:65536" >> /etc/subgid
# Preconfigure containers storage + engine so buildah/podman work out of the box
# in a privileged pod: vfs driver, chroot isolation, writable graphroot/runroot
# under /data, netavark network backend (bridge + built-in aardvark DNS).
RUN mkdir -p /etc/containers && cat > /etc/containers/storage.conf <<'CONF'
[storage]
driver = "vfs"
runroot = "/data/runroot"
graphroot = "/data/graphroot"
CONF
RUN cat > /etc/containers/containers.conf <<'CONF'
[engine]
helper_binaries_dir = ["/usr/libexec/podman", "/usr/bin"]
network_backend = "netavark"
CONF
# buildah defaults to the internal /v2 registry: unqualified names (e.g. "busybox")
# resolve to the built-in registry (127.0.0.1:8080, plain HTTP in-cluster) instead
# of docker.io, so base images are pulled from the internal cache. Listed as
# insecure so containers/image uses HTTP (no TLS).
RUN cat > /etc/containers/registries.conf <<'CONF'
unqualified-search-registries = ["127.0.0.1:8080"]

[[registry]]
location = "127.0.0.1:8080"
insecure = true
CONF
# buildah/podman (embedded) are the ONLY runtime builders/launchers.
# EASYVCS_HOME=/data is writable (hostPath -> the host /home/develop/.easylab)
# and holds buildah/podman containers-store so layers persist and the OCI
# registry cache is reusable.
COPY --from=gobuild /out/easylab /usr/local/bin/easylab
COPY --from=gobuild /out/easyvcs /usr/local/bin/easyvcs
# entrypoint: remount /sys/fs/cgroup read-write so podman/crun can create
# child cgroups for real per-service CPU/memory limits. Requires the pod to
# run privileged (SYS_ADMIN) — see the deployment manifest.
RUN cat > /usr/local/bin/easylab-entrypoint.sh <<'EOF'
#!/bin/sh
# Best-effort: make cgroup writable for podman. Non-fatal if already rw.
mount -o remount,rw /sys/fs/cgroup 2>/dev/null || true
exec /usr/local/bin/easylab "$@"
EOF
RUN chmod +x /usr/local/bin/easylab-entrypoint.sh
ENV EASYVCS_HOME=/data \
    EASYVCS_SELF_BASE="" \
    EASYVCS_OPS_NAMESPACES=default \
    EASYVCS_BUILD_BACKEND=buildah \
    EASYVCS_BUILDAH_BIN=/usr/bin/buildah \
    EASYVCS_BUILDAH_TMP=/data/buildtmp \
    EASYVCS_REGISTRY=127.0.0.1:8080 \
    EASYVCS_PODMAN_BIN=/usr/bin/podman \
    _CONTAINERS_USERNS_CONFIGURED=1 \
    BUILDAH_ISOLATION=chroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/easylab-entrypoint.sh"]
CMD ["-addr", "0.0.0.0:8080"]
