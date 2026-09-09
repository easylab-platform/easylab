#!/usr/bin/env bash
# Multi-image sandbox matrix e2e: verifies the easylab SandboxService launches
# worker-backed sandboxes from arbitrary distro/toolchain base images, runs a
# job, and cleans up. Run against the in-cluster easylab gateway (temp ns).
set -euo pipefail
EAS="${EAS:-http://easylab.temp.svc.cluster.local}"
SB=easylab.v1.SandboxService

declare -a IMAGES=(
  docker.io/library/debian:12
  docker.io/library/alpine:3.20
  docker.io/library/fedora:40
  docker.io/library/golang:1.26-alpine
  docker.io/library/rust:1
  docker.io/library/node:22-alpine
)

fail=0
for img in "${IMAGES[@]}"; do
  n="sbx-mx-$(echo "$img" | tr ':/.' '_' | cut -c20-40)"
  echo "===== $img ($n) ====="
  r=$(curl -s -m 300 -X POST -H 'Content-Type: application/json' -d "{\"name\":\"$n\",\"baseImage\":\"$img\"}" "$EAS/$SB/LaunchSandbox")
  ph=$(echo "$r" | python3 -c 'import json,sys
try:
  print(json.load(sys.stdin)["sandbox"]["phase"])
except Exception:
  print("LAUNCH_ERR")' 2>/dev/null)
  echo "  phase: $ph"
  j=""
  if [ "$ph" = "Running" ]; then
    b=$(curl -s -m 20 -X POST -H 'Content-Type: application/json' -d "{\"name\":\"$n\"}" "$EAS/$SB/GetSandbox" \
      | python3 -c 'import json,sys; print(json.load(sys.stdin)["sandbox"]["bootId"][:8])' 2>/dev/null || echo "?")
    echo "  bootId: $b"
    j=$(curl -s -m 20 -X POST -H 'Content-Type: application/json' -d "{\"sandbox\":\"$n\",\"req\":{\"command\":\"echo ok-$n\"}}" "$EAS/$SB/Execute" \
      | python3 -c 'import json,sys; print(json.load(sys.stdin)["jobId"])' 2>/dev/null || echo "")
    sleep 1.5
    out=$(curl -s -m 20 -X POST -H 'Content-Type: application/json' \
      -d "{\"sandbox\":\"$n\",\"req\":{\"jobId\":\"$j\",\"start\":0,\"end\":0}}" "$EAS/$SB/JobOutput" \
      | python3 -c 'import json,sys
try: print(json.load(sys.stdin)["lines"][0])
except Exception: print("<none>")' 2>/dev/null)
    if [ "$out" = "ok-$n" ]; then echo "  out: $out ✓"; else echo "  out: $out ✗"; fail=1; fi
  else
    fail=1
  fi
  curl -s -m 30 -X POST -H 'Content-Type: application/json' -d "{\"name\":\"$n\"}" "$EAS/$SB/DeleteSandbox" >/dev/null
done

echo "====================================="
if [ $fail -eq 0 ]; then echo "MATRIX: ALL PASS"; else echo "MATRIX: FAILURES"; fi
exit $fail
