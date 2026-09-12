# syntax=docker/dockerfile:1
# EasyLab buildkit-worker: ONE container that runs both a rootless buildkitd
# (background) and the easyworker (PID 1). easylab drives builds exactly like
# every other job — it launches this image as a worker job and runs `buildctl`
# through the worker API. No sidecar, no shared unix socket volume, no hostPath
# meta files.
#
# Base: the cluster's rootless buildkit image (buildkitd + buildctl + runc),
# which is reachable from the node's containerd without extra registry config.
ARG REGISTRY=forgejo.develop.10.199.64.20.nip.io/root
ARG BUILDKIT_VERSION=v0.32.2-rootless

FROM ${REGISTRY}/buildkit:${BUILDKIT_VERSION}

USER root
COPY --chown=1000:1000 worker-bin/easyworker /usr/local/bin/easyworker
COPY --chown=1000:1000 buildkit/entrypoint.sh /usr/local/bin/easylab-buildkit-entrypoint.sh
RUN chmod +x /usr/local/bin/easylab-buildkit-entrypoint.sh /usr/local/bin/easyworker \
    && mkdir -p /workspace /home/user/.docker /home/user/.config/buildkit \
    && chown -R 1000:1000 /workspace /home/user/.docker /home/user/.config

USER 1000:1000
ENV WORKER_PORT=48080 \
    WORKER_WORKSPACE=/workspace \
    WORKER_DB=/tmp/easyworker.db \
    DOCKER_CONFIG=/home/user/.docker \
    XDG_RUNTIME_DIR=/run/user/1000 \
    BUILDKIT_HOST=unix:///run/user/1000/buildkit/buildkitd.sock
EXPOSE 48080
ENTRYPOINT ["/usr/local/bin/easylab-buildkit-entrypoint.sh"]
