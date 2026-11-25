#!/usr/bin/env bash
# End-to-end check of the demo stack (compose.yaml) next to the dashboard's dev stack. Both must
# be up:
#
#   (cd ../dashboard-v2 && docker compose -f compose.dev.yml up -d --build)
#   docker compose up -d --build
#   deploy/smoke.sh
#
# Logs in at the dashboard's mock system manager, places a model through the admin API, chats
# with it through /v1 using a fresh API key, checks the UI proxy chain through the dashboard and
# cleans up. Leftovers of an interrupted run (names starting with "smoke-") are removed first.
#
# Needs bash, curl and jq. Ports follow compose.yaml and the dashboard's compose.dev.yml.
set -euo pipefail

admin="http://127.0.0.1:${OOA_ADMIN_PORT:-8096}/admin/v1"
public="http://127.0.0.1:${OOA_PORT:-8095}"
login_url="http://localhost:${MOCK_API_PORT:-10010}/api/auth/login"
dashboard="http://localhost:${DASHBOARD_PORT:-8080}"
user="${SMOKE_USER:-Admin}"
password="${SMOKE_PASSWORD:-Admin}"
timeout="${SMOKE_TIMEOUT:-120}"

command -v jq >/dev/null || { echo "smoke: FAIL: jq is required" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

fail() { echo "smoke: FAIL: $*" >&2; exit 1; }
step() { echo "smoke: $*"; }

# call METHOD URL [JSON body] [extra curl args...]: sets status and body. Never fails by
# itself, so every check states the status it expects.
call() {
  local method=$1 url=$2 data=${3:-}
  shift $(( $# < 3 ? $# : 3 ))
  local args=(-sS -X "$method" -o "$tmp/body" -w '%{http_code}' "$@")
  [ -n "$data" ] && args+=(-H 'Content-Type: application/json' --data "$data")
  status=$(curl "${args[@]}" "$url") || status=000
  body=$(cat "$tmp/body" 2>/dev/null || true)
}

# expect STATUS WHAT: fails unless the last call answered STATUS.
expect() {
  [ "$status" = "$1" ] || fail "$2: HTTP $status, want $1: $body"
}

# wait_for DESCRIPTION COMMAND...: retries COMMAND until it succeeds or the timeout passes.
wait_for() {
  local what=$1 end=$(( $(date +%s) + timeout ))
  shift
  until "$@"; do
    [ "$(date +%s)" -lt "$end" ] || fail "$what not reached after ${timeout}s (last: HTTP $status $body)"
    sleep 2
  done
}

up() { call GET "$1"; [ "$status" = 200 ]; }
wait_for "public listener" up "$public/readyz"

step "logging in as $user at the mock system manager"
call POST "$login_url" "$(jq -nc --arg u "$user" --arg p "$password" '{username: $u, password: $p}')"
expect 200 "login"
token=$(jq -r '.token // empty' <<<"$body")
[ -n "$token" ] || fail "login returned no token: $body"
bearer=(-H "Authorization: Bearer $token")

# The admin calls below all carry the dashboard token.
acall() { local m=$1 u=$2 d=${3:-}; call "$m" "$admin$u" "$d" "${bearer[@]}"; }

acall GET /me
expect 200 "GET /me"
[ "$(jq -r '.name' <<<"$body")" = "$user" ] && [ "$(jq -r '.admin' <<<"$body")" = true ] ||
  fail "/me is not $user as admin: $body"
step "admin API knows us: $body"

step "removing leftovers of earlier runs"
acall GET /deployments
expect 200 "GET /deployments"
for id in $(jq -r '.deployments[] | select(.model_name | startswith("smoke-")) | .id' <<<"$body"); do
  acall DELETE "/deployments/$id"
  [ "$status" = 202 ] || [ "$status" = 404 ] || fail "removing stale deployment $id: HTTP $status $body"
done
no_smoke_deployments() {
  acall GET /deployments
  [ "$status" = 200 ] && [ "$(jq '[.deployments[] | select(.model_name | startswith("smoke-"))] | length' <<<"$body")" = 0 ]
}
wait_for "stale deployments removed" no_smoke_deployments
acall GET /models
for id in $(jq -r '.models[] | select(.name | startswith("smoke-")) | .id' <<<"$body"); do
  acall DELETE "/models/$id"
  expect 204 "removing stale model $id"
done
acall GET /keys
for id in $(jq -r '.keys[] | select(.name | startswith("smoke-")) | .id' <<<"$body"); do
  acall DELETE "/keys/$id"
  expect 204 "revoking stale key $id"
done

acall GET /nodes
expect 200 "GET /nodes"
[ "$(jq '.nodes | length' <<<"$body")" = 3 ] || fail "want 3 nodes: $body"
[ "$(jq '[.nodes[] | select(.modelpack_csi | not)] | length' <<<"$body")" = 1 ] ||
  fail "want exactly one node without the modelpack driver: $body"
cluster=$(jq -r '[.nodes[] | select(.modelpack_csi)][0].cluster' <<<"$body")
good=$(jq -r '[.nodes[] | select(.modelpack_csi)][0].name' <<<"$body")
plain=$(jq -r '[.nodes[] | select(.modelpack_csi | not)][0].name' <<<"$body")
step "nodes: 3 ($plain lacks the driver), deploying on $cluster/$good"

name="smoke-$(date +%s)"
acall POST /models "$(jq -nc --arg n "$name" '{name: $n, reference: "registry.example/models/smoke:1", memory_mb: 1024}')"
expect 201 "POST /models"
model=$(jq -r '.id' <<<"$body")
step "model $name created ($model)"

acall POST /deployments "$(jq -nc --arg m "$model" --arg c "$cluster" --arg n "$plain" '{model_id: $m, cluster: $c, node: $n}')"
expect 422 "deploying on $plain, which has no modelpack driver"
step "deploying on $plain is refused: $(jq -r '.error.message' <<<"$body")"

acall POST /deployments "$(jq -nc --arg m "$model" --arg c "$cluster" --arg n "$good" '{model_id: $m, cluster: $c, node: $n}')"
expect 201 "POST /deployments"
deployment=$(jq -r '.id' <<<"$body")
ready() {
  acall GET /deployments
  local state
  state=$(jq -r --arg id "$deployment" '.deployments[] | select(.id == $id) | .state' <<<"$body")
  [ "$state" != failed ] || fail "deployment failed: $body"
  [ "$state" = ready ]
}
wait_for "deployment $deployment ready" ready
step "deployment $deployment is ready"

acall POST /keys "$(jq -nc --arg n "$name" '{name: $n}')"
expect 201 "POST /keys"
key_id=$(jq -r '.key.id' <<<"$body")
secret=$(jq -r '.secret' <<<"$body")
key=(-H "Authorization: Bearer $secret")
step "API key $key_id created"

call GET "$public/v1/models"
expect 401 "GET /v1/models without a key"
call GET "$public/v1/models" "" "${key[@]}"
expect 200 "GET /v1/models"
for id in "$name" auto; do
  jq -e --arg id "$id" 'any(.data[]; .id == $id)' <<<"$body" >/dev/null || fail "/v1/models lacks $id: $body"
done
step "/v1/models lists $name and auto"

call POST "$public/v1/chat/completions" \
  "$(jq -nc --arg m "$name" '{model: $m, messages: [{role: "user", content: "hello smoke"}]}')" "${key[@]}"
expect 200 "non-streaming chat with $name"
reply=$(jq -r '.choices[0].message.content // empty' <<<"$body")
[ -n "$reply" ] || fail "chat answer has no content: $body"
[ "$(jq -r '.model' <<<"$body")" = "$name" ] || fail "chat answered for another model: $body"
step "chat with $name: \"$reply\""

call POST "$public/v1/chat/completions" \
  '{"model":"auto","stream":true,"messages":[{"role":"user","content":"hello smoke"}]}' "${key[@]}" -N
expect 200 "streaming chat with auto"
chunks=$(grep -c '^data: {' <<<"$body" || true)
[ "$chunks" -ge 2 ] || fail "expected several SSE chunks: $body"
grep -q '^data: \[DONE\]' <<<"$body" || fail "stream did not end with [DONE]: $body"
step "streaming chat with auto: $chunks chunks and [DONE]"

call POST "$public/v1/chat/completions" '{"model":"no-such-model","messages":[{"role":"user","content":"x"}]}' "${key[@]}"
expect 404 "chat with an unknown model"
[ "$(jq -r '.error.code' <<<"$body")" = model_not_found ] && [ "$(jq -r '.error.type' <<<"$body")" = invalid_request_error ] ||
  fail "unknown model error is not OpenAI-shaped: $body"
step "unknown model: 404 model_not_found"

call GET "$public/admin/v1/models" "" "${bearer[@]}"
expect 404 "admin API on the public port"
step "admin API is not served on the public port"

call GET "$dashboard/addon-assets/openai-api/mf-manifest.json"
expect 200 "addon manifest through the dashboard"
call GET "$dashboard/addon-assets/openai-api/api/v1/me" "" "${bearer[@]}"
expect 200 "admin API through the dashboard and the addon UI"
[ "$(jq -r '.name' <<<"$body")" = "$user" ] || fail "/me through the dashboard: $body"
step "dashboard -> addon UI -> admin API chain works"

acall DELETE "/deployments/$deployment"
expect 202 "DELETE /deployments/$deployment"
gone() {
  acall GET /deployments
  [ "$status" = 200 ] && [ "$(jq --arg id "$deployment" '[.deployments[] | select(.id == $id)] | length' <<<"$body")" = 0 ]
}
wait_for "deployment $deployment removed" gone
step "deployment removed"

acall DELETE "/keys/$key_id"
expect 204 "DELETE /keys/$key_id"
call GET "$public/v1/models" "" "${key[@]}"
expect 401 "GET /v1/models with the revoked key"
step "revoked key is refused"

acall DELETE "/models/$model"
expect 204 "DELETE /models/$model"
echo "smoke: ok"
