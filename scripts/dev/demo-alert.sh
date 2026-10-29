#!/usr/bin/env bash
# Posts a KubePodCrashLooping alert for the checkout pod that is crash looping
# on the kind cluster, as Alertmanager would.
set -euo pipefail

kubectl=(kubectl --context "kind-${KIND_CLUSTER:-themis-dev}" -n shop)
pod=$("${kubectl[@]}" get pods -l app=checkout -o jsonpath='{range .items[*]}{.metadata.name} {.status.containerStatuses[0].state.waiting.reason}{"\n"}{end}' |
  awk '$2 == "CrashLoopBackOff" {print $1; exit}')
if [ -z "$pod" ]; then
  echo "no checkout pod is crash looping yet: run make demo-break and wait" >&2
  exit 1
fi

now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
curl -sS -X POST "${THEMIS_URL:-http://127.0.0.1:8080}/v1/intake/alertmanager" \
  -H "Authorization: Bearer $(cat "${TOKEN_FILE:-bin/dev/intake-token}")" \
  -H "Content-Type: application/json" \
  --data @- <<JSON
{"version": "4", "status": "firing", "receiver": "themis", "groupKey": "demo",
 "alerts": [{"status": "firing", "fingerprint": "demo-$pod",
   "labels": {"alertname": "KubePodCrashLooping", "namespace": "shop", "pod": "$pod", "container": "app", "severity": "warning"},
   "annotations": {"summary": "Pod is crash looping."},
   "startsAt": "$now", "endsAt": "0001-01-01T00:00:00Z"}]}
JSON
echo
