#!/bin/sh
# EasyLab buildkit-worker entrypoint: prepare registry auth + buildkitd config
# from the per-job env, start rootless buildkitd in the background, then run
# the easyworker as PID 1. easylab drives builds by executing `buildctl`
# through the worker API, exactly like any other job command.
set -e

REGISTRY="${EASYLAB_REGISTRY_HOST:-}"
TOKEN="${EASYLAB_REGISTRY_TOKEN:-}"
XDG="${XDG_RUNTIME_DIR:-/run/user/1000}"
SOCK="$XDG/buildkit/buildkitd.sock"
CFG="$HOME/.config/buildkit/buildkitd.toml"

mkdir -p "$HOME/.config/buildkit" "$XDG/buildkit" "$DOCKER_CONFIG" /workspace

if [ -n "$REGISTRY" ]; then
  # buildkitd trusts the easylab registry over plain HTTP (in-cluster Service).
  cat > "$CFG" <<EOF
[registry."$REGISTRY"]
  http = true
  insecure = true
EOF
  # buildctl's authprovider forwards these credentials to buildkitd for push.
  AUTH=$(printf 'agent:%s' "$TOKEN" | base64 | tr -d '\n')
  cat > "$DOCKER_CONFIG/config.json" <<EOF
{"auths":{"$REGISTRY":{"auth":"$AUTH"}}}
EOF
else
  : > "$CFG"
fi

# Rootless buildkitd in the background (the image ships rootlesskit + runc).
# --oci-worker-no-process-sandbox is safe here: the pod bounds the build.
if [ -n "$REGISTRY" ]; then
  rootlesskit buildkitd --addr "unix://$SOCK" --config "$CFG" \
    --oci-worker-no-process-sandbox >/tmp/buildkitd.log 2>&1 &
else
  rootlesskit buildkitd --addr "unix://$SOCK" \
    --oci-worker-no-process-sandbox >/tmp/buildkitd.log 2>&1 &
fi

# Wait for the daemon socket so the first buildctl doesn't race it.
i=0
while [ ! -S "$SOCK" ]; do
  i=$((i + 1))
  [ "$i" -gt 100 ] && break
  sleep 0.1
done

export BUILDKIT_HOST="unix://$SOCK"
exec /usr/local/bin/easyworker --addr "0.0.0.0:${WORKER_PORT:-48080}"
