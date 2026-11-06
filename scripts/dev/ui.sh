#!/usr/bin/env bash
# Serves the in-cluster Themis UI on http://localhost:8090 until Ctrl-C. A
# port-forward ends when the pod behind it goes, as on every helm upgrade:
# this one waits until themis-core is ready again and reconnects.
set -euo pipefail

port=8090
kubectl=(kubectl --context "kind-${KIND_CLUSTER:-themis-dev}" -n themis)
trap 'exit 0' INT TERM

if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
  echo "Port $port is in use, perhaps by another make ui. Stop it first." >&2
  exit 1
fi

while :; do
  if ! "${kubectl[@]}" rollout status deployment/themis-core --timeout=10m >/dev/null; then
    sleep 5
    continue
  fi
  "${kubectl[@]}" port-forward deploy/themis-core "$port:8080" || true
  echo "The port-forward ended. Reconnecting once themis-core is ready." >&2
  sleep 1
done
