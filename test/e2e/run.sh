#!/usr/bin/env bash
# End-to-end test of the bad-rollout scenario on kind: real Alertmanager,
# Themis with the executor, and a fake model. It asserts on incident states
# and the audit trail, never on model wording.
#
#   test/e2e/run.sh            create the cluster if needed, install, test
#   KEEP=1 test/e2e/run.sh     leave everything running afterwards
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"
CLUSTER=${KIND_CLUSTER:-themis-e2e}
CTX=kind-$CLUSTER
K="kubectl --context $CTX"
KIND="go tool -modfile=tools/go.mod kind"
ART=test/e2e/artifacts
PORT=18180
API=http://127.0.0.1:$PORT
PASSED=0
FAILED=0
mkdir -p "$ART"

log() { printf '\n== %s\n' "$*"; }
pass() { printf '  PASS %s\n' "$*"; PASSED=$((PASSED + 1)); }
fail() { printf '  FAIL %s\n' "$*"; FAILED=$((FAILED + 1)); }
check() { # check <description> <command...>
  local what=$1; shift
  if "$@"; then pass "$what"; else fail "$what"; fi
}

collect() {
  for d in themis-core themis-executor themis-intelligence fake-model webhook-sink; do
    $K -n themis logs deploy/$d --tail=300 > "$ART/$d.log" 2>&1 || true
  done
  $K -n monitoring logs statefulset/monitoring-alertmanager -c alertmanager --tail=200 > "$ART/alertmanager.log" 2>&1 || true
}

finish() {
  collect
  [ -n "${PF:-}" ] && kill "$PF" 2>/dev/null || true
  printf '\n%d passed, %d failed. Logs in %s\n' "$PASSED" "$FAILED" "$ART"
  if [ -z "${KEEP:-}" ] && [ -z "${KIND_CLUSTER:-}" ]; then $KIND delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true; fi
}
trap finish EXIT

# --- API helpers -------------------------------------------------------------

token() { $K -n themis get secret themis-tokens -o jsonpath="{.data.$1}" | base64 -d; }
api() { curl -sf -H "Authorization: Bearer $ADMIN" "$API$1"; }
post() { curl -s -o /dev/null -w '%{http_code}' -X POST -H "Authorization: Bearer $ADMIN" "$API$1" -d "$2"; }
# field evaluates an expression written in this script over the JSON on stdin.
field() { python3 -c "import json,sys; d=json.load(sys.stdin); print(eval(sys.argv[1]))" "$1"; }
latest() { api /v1/incidents | field 'd[0]["id"] if d else ""'; }
state() { api "/v1/incidents/$1" | field 'd["Incident"]["state"]'; }

# wait_state <id> <timeout-seconds> <state...>
wait_state() {
  local id=$1 deadline=$((SECONDS + $2)); shift 2
  while [ $SECONDS -lt $deadline ]; do
    local s; s=$(state "$id")
    for want in "$@"; do [ "$s" = "$want" ] && { echo "$s"; return 0; }; done
    sleep 3
  done
  state "$id"
}

# heal restores the healthy checkout.
heal() {
  $K -n shop set env deploy/checkout MODE=healthy EXTRA- >/dev/null
  $K -n shop annotate deploy/checkout argocd.argoproj.io/tracking-id- >/dev/null 2>&1 || true
  $K -n shop rollout status deploy/checkout --timeout=180s >/dev/null
}

# incident_after_break heals, breaks checkout, and prints the incident
# Alertmanager opens for it.
incident_after_break() {
  local before; before=$(latest)
  heal
  $K -n shop set env deploy/checkout MODE=leak >/dev/null
  local deadline=$((SECONDS + 240)) id=$before
  while [ "$id" = "$before" ] && [ $SECONDS -lt $deadline ]; do sleep 5; id=$(latest); done
  [ "$id" != "$before" ] && echo "$id"
}

action() { api "/v1/incidents/$1" | field "d['Actions'][-1]$2"; }
approve() { post "/v1/actions/$(action "$1" '["ID"]')/approve" "{\"digest\":\"$(action "$1" '["Digest"]')\"}"; }

# --- Setup -------------------------------------------------------------------

log "cluster $CLUSTER"
$KIND get clusters 2>/dev/null | grep -qx "$CLUSTER" || $KIND create cluster --name "$CLUSTER" --config test/e2e/kind-config.yaml --wait 2m
make -s kind-load KIND_CLUSTER="$CLUSTER" >/dev/null

log "workload, model, monitoring, Themis"
$K apply -f hack/dev/demo/checkout.yaml >/dev/null
$K -n shop rollout status deploy/checkout --timeout=180s >/dev/null
$K create namespace themis --dry-run=client -o yaml | $K apply -f - >/dev/null
$K -n themis create configmap fake-model --from-file=test/e2e/fake_model.py --dry-run=client -o yaml | $K apply -f - >/dev/null
$K -n themis create secret generic e2e-webhook --from-literal=url=http://webhook-sink.themis.svc:8080/ --from-literal=token=e2e-token \
  --dry-run=client -o yaml | $K apply -f - >/dev/null
$K -n themis apply -f test/e2e/manifests.yaml >/dev/null
helm --kube-context "$CTX" upgrade --install themis deploy/helm/themis -n themis -f test/e2e/values.yaml --wait --timeout 5m >/dev/null
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
make -s monitoring-up KIND_CLUSTER="$CLUSTER" >/dev/null
$K -n themis rollout status deploy/fake-model deploy/webhook-sink --timeout=180s >/dev/null

$K -n themis port-forward svc/themis-core $PORT:8080 >/dev/null 2>&1 &
PF=$!
sleep 3
ADMIN=$(token admin)
INTAKE=$(token intake)

# --- Intake ------------------------------------------------------------------

log "intake"
code() { curl -s -o /dev/null -w '%{http_code}' -X POST "$API/v1/intake/alertmanager" "$@"; }
check "a webhook without a token is refused" [ "$(code -d '{}')" = 401 ]
check "an unknown payload version is refused" [ "$(code -H "Authorization: Bearer $INTAKE" -d '{"version":"3","alerts":[]}')" = 400 ]
check "the API needs the admin token" [ "$(curl -s -o /dev/null -w '%{http_code}' "$API/v1/incidents")" = 401 ]

# --- Scenarios ---------------------------------------------------------------

log "1. a bad rollout is rolled back after approval"
ID=$(incident_after_break)
check "Alertmanager's alert opened an incident" [ -n "$ID" ]
check "the investigation proposes a fix that awaits approval" [ "$(wait_state "$ID" 300 awaiting_approval closed_no_action escalated)" = awaiting_approval ]
check "the action shows that MODE changes" [ "$(action "$ID" '["Changes"][0]["field"]')" = "env MODE" ]
check "approving the shown digest is accepted" [ "$(approve "$ID")" = 200 ]
check "the rollback is verified as resolved" [ "$(wait_state "$ID" 300 resolved escalated contained_escalated)" = resolved ]
check "checkout runs the healthy revision again" [ "$($K -n shop get deploy checkout -o jsonpath='{.spec.template.spec.containers[0].env[0].value}')" = healthy ]

log "2. a denied action is never executed"
ID=$(incident_after_break)
wait_state "$ID" 300 awaiting_approval >/dev/null
check "denying is accepted" [ "$(post "/v1/actions/$(action "$ID" '["ID"]')/deny" '{"reason":"e2e"}')" = 200 ]
check "the incident closes without action" [ "$(state "$ID")" = closed_no_action ]
check "approving after a denial is refused" [ "$(approve "$ID")" = 409 ]
check "checkout is untouched" [ "$($K -n shop get deploy checkout -o jsonpath='{.spec.template.spec.containers[0].env[0].value}')" = leak ]

log "3. a GitOps-managed target is not changed"
heal
$K -n shop annotate deploy/checkout argocd.argoproj.io/tracking-id=shop:apps/Deployment:shop/checkout --overwrite >/dev/null
BEFORE=$(latest)
$K -n shop set env deploy/checkout MODE=leak >/dev/null
DEADLINE=$((SECONDS + 240)); ID=$BEFORE
while [ "$ID" = "$BEFORE" ] && [ $SECONDS -lt $DEADLINE ]; do sleep 5; ID=$(latest); done
check "policy denies the action" [ "$(wait_state "$ID" 300 closed_no_action awaiting_approval escalated)" = closed_no_action ]
check "the denying rule is gitops-owns-the-target" [ "$(action "$ID" '["PolicyID"]')" = gitops-owns-the-target ]

log "4. a human edit during verification makes the outcome inconclusive"
ID=$(incident_after_break)
wait_state "$ID" 300 awaiting_approval >/dev/null
approve "$ID" >/dev/null
wait_state "$ID" 120 verifying >/dev/null
$K -n shop set env deploy/checkout EXTRA=human >/dev/null
check "the incident is escalated" [ "$(wait_state "$ID" 300 escalated resolved)" = escalated ]
check "verification was inconclusive" [ "$(action "$ID" '["Verification"]["Outcome"]')" = inconclusive ]
sleep 5
check "the escalation was posted to the webhook" bash -c "$K -n themis logs deploy/webhook-sink | grep -q '\"id\":\"$ID\"'"
check "the webhook got its bearer token" bash -c "$K -n themis logs deploy/webhook-sink | grep -q 'AUTH Bearer e2e-token'"

log "5. a fix made outside Themis closes the incident"
ID=$(incident_after_break)
wait_state "$ID" 300 awaiting_approval >/dev/null
$K -n shop set env deploy/checkout MODE=healthy >/dev/null
check "the incident self-resolves" [ "$(wait_state "$ID" 240 self_resolved canceled escalated)" = self_resolved ]
check "its action is canceled" [ "$(action "$ID" '["State"]')" = canceled ]

log "6. a change outside Themis that does not help supersedes the incident"
ID=$(incident_after_break)
wait_state "$ID" 300 awaiting_approval >/dev/null
$K -n shop set env deploy/checkout EXTRA=unhelpful >/dev/null
check "the incident is canceled" [ "$(wait_state "$ID" 240 canceled self_resolved escalated)" = canceled ]
NEXT=$(api "/v1/incidents/$ID" | field 'd["Incident"].get("supersededBy","")')
check "a new incident takes its place" [ -n "$NEXT" ]
check "the new incident is investigated" [ "$(wait_state "$NEXT" 300 awaiting_approval escalated closed_no_action)" = awaiting_approval ]

heal
[ "$FAILED" -eq 0 ]
