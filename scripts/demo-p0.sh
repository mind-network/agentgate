#!/usr/bin/env bash
set -euo pipefail
# demo-p0.sh — Phase 0 automatic demo against DeepSeek upstream
# Requires: Docker, Go, curl, openssl, psql
# Preflight: DEEPSEEK_API_KEY must be set in the environment.

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ORIGINAL_HOME="${HOME}"

GW_URL="https://localhost:8443"
LP_PORT=9777
TMPDIR="${ROOT}/.demo-p0-tmp"
SERVICES_STARTED=false
DEMO_SUCCESS=false

# --- helpers ----------------------------------------------------------------
bold()   { echo -e "\033[1m$*\033[0m"; }
dim()    { echo -e "\033[2m$*\033[0m"; }
red()    { echo -e "\033[31m$*\033[0m" >&2; }
green()  { echo -e "\033[32m$*\033[0m"; }

section() {
  echo ""
  bold "=== $1 ==="
}

check_cmd() {
  command -v "$1" >/dev/null 2>&1 || { red "MISSING: $1 — please install it"; return 1; }
}

cleanup() {
  echo ""
  dim "--- Cleanup ---"
  cd "$ROOT"

  if [ "$SERVICES_STARTED" = "true" ] && command -v docker >/dev/null 2>&1; then
    docker compose down -v 2>/dev/null || docker-compose down -v 2>/dev/null || true
  fi

  # Kill LP if running in background.
  if [ -n "${LP_PID:-}" ]; then
    kill "$LP_PID" 2>/dev/null || true
    wait "$LP_PID" 2>/dev/null || true
  fi

  if [ "$DEMO_SUCCESS" = "true" ]; then
    chmod -R u+w "$TMPDIR" 2>/dev/null || true
    rm -rf "$TMPDIR"
    dim "Cleanup done."
  else
    dim "Cleanup: preserving $TMPDIR for debugging"
  fi
}
trap cleanup EXIT

fail_step() {
  local step="$1"
  local detail="${2:-}"
  red ""
  red "DEMO FAILED at step: $step"
  red "Log dir:  $TMPDIR"
  if [ -n "$detail" ]; then
    red "Detail:   $detail"
  fi
  exit 1
}

# --- preflight --------------------------------------------------------------
section "0. Preflight"

check_cmd docker
check_cmd go
check_cmd curl
check_cmd openssl
check_cmd psql

if [ -z "${DEEPSEEK_API_KEY:-}" ]; then
  red ""
  red "DEEPSEEK_API_KEY is not set."
  red "This demo requires a real DeepSeek API key because P0 uses DeepSeek as the"
  red "default upstream provider. Obtain your key from https://platform.deepseek.com"
  red "and re-run with:"
  red ""
  red "  export DEEPSEEK_API_KEY='sk-...'"
  red "  bash scripts/demo-p0.sh"
  red ""
  red "Without a key the demo cannot prove the real-provider path."
  exit 1
fi

# Validate key shape (basic: non-empty, no shell-significant chars leaking).
if ! echo "$DEEPSEEK_API_KEY" | grep -qE '^sk-'; then
  red "WARNING: DEEPSEEK_API_KEY does not start with 'sk-'. The upstream may reject it."
fi

green "Preflight OK"

# Resolve docker compose command.
DOCKER_COMPOSE=""
if docker compose version >/dev/null 2>&1; then
  DOCKER_COMPOSE="docker compose"
elif docker-compose version >/dev/null 2>&1; then
  DOCKER_COMPOSE="docker-compose"
else
  red "Neither 'docker compose' nor 'docker-compose' found."
  exit 1
fi

# --- setup temp dir ---------------------------------------------------------
rm -rf "$TMPDIR"
mkdir -p "$TMPDIR"

# --- 1. Build binaries ------------------------------------------------------
section "1. Build binaries"
go build -o "$TMPDIR/aicg-gw" ./cmd/aicg-gw
go build -o "$TMPDIR/aicg-lp" ./cmd/aicg-lp
green "Binaries built"

# --- 2. Generate dev certs ---------------------------------------------------
section "2. Generate TLS certificates"
bash "$ROOT/scripts/gen-dev-cert.sh"
green "Certificates ready"

# --- 3. Isolated runtime config ----------------------------------------------
section "3. Isolated runtime config"

mkdir -p "$TMPDIR/configs/policies"
mkdir -p "$TMPDIR/configs/identity"

# Copy repo configs into isolated temp dir (do not edit repo files).
cp "$ROOT/configs/pools.yaml" "$TMPDIR/configs/pools.yaml"
cp "$ROOT/configs/pricing.yaml" "$TMPDIR/configs/pricing.yaml"
cp "$ROOT/configs/policies/main.yaml" "$TMPDIR/configs/policies/main.yaml"
cp "$ROOT/configs/identity/users.yaml" "$TMPDIR/configs/identity/users.yaml"
cp "$ROOT/configs/identity/teams.yaml" "$TMPDIR/configs/identity/teams.yaml"
cp "$ROOT/configs/identity/repos.yaml" "$TMPDIR/configs/identity/repos.yaml"

green "Runtime config copied to $TMPDIR/configs"

# --- 4. Start services (Postgres + GW) --------------------------------------
section "4. Start services"

export AICG_EXTRA_CONFIG_DIR="$TMPDIR/configs"

# Pass DEEPSEEK_API_KEY through to Docker GW.
$DOCKER_COMPOSE up -d --build 2>&1 | tail -5

# Wait for postgres.
for i in $(seq 1 20); do
  if $DOCKER_COMPOSE ps 2>/dev/null | grep -q 'postgres.*Up'; then
    break
  fi
  sleep 1
done
green "Postgres running"
SERVICES_STARTED=true

# --- 5. Wait for GW healthy -------------------------------------------------
section "5. Wait for Gateway"

for i in $(seq 1 30); do
  if curl --cacert "$ROOT/certs/ca.crt" -sk "$GW_URL/healthz" 2>/dev/null | grep -q '"db":"ok"'; then
    break
  fi
  sleep 1
done
GW_HEALTH=$(curl --cacert "$ROOT/certs/ca.crt" -sk "$GW_URL/healthz" 2>/dev/null || echo '{"status":"error"}')
if ! echo "$GW_HEALTH" | grep -q '"db":"ok"'; then
  fail_step "GW health" "GW did not become healthy: $GW_HEALTH"
fi
green "Gateway healthy"
dim "  $GW_HEALTH"

# --- 6. Get setup token & exchange for API key ------------------------------
section "6. Exchange setup token"

GW_CID=$($DOCKER_COMPOSE ps -q gw 2>/dev/null | head -1)
SETUP_TOKEN=$(docker exec "$GW_CID" cat /tmp/aicg-setup-token 2>/dev/null | tr -d '\n\r' || true)
if [ -z "$SETUP_TOKEN" ] || ! echo "$SETUP_TOKEN" | grep -Eq '^setup_[[:xdigit:]]{64}$'; then
  # Fallback: scan GW logs.
  SETUP_TOKEN=$($DOCKER_COMPOSE logs gw 2>/dev/null | grep -Eo 'setup_[[:xdigit:]]{64}' | head -1 | tr -d '\n\r' || true)
fi

if [ -z "$SETUP_TOKEN" ] || ! echo "$SETUP_TOKEN" | grep -Eq '^setup_[[:xdigit:]]{64}$'; then
  fail_step "setup token" "Could not extract valid setup_token"
fi
green "Setup token obtained"

# Exchange for API key.
export HOME="$TMPDIR"
"$TMPDIR/aicg-lp" login --setup "$SETUP_TOKEN" --gateway "$GW_URL" --ca "$ROOT/certs/ca.crt" > "$TMPDIR/login.txt" 2>&1
API_KEY=$(grep '^api_key:' "$TMPDIR/.aicg/credentials" | awk '{print $2}' | tr -d '\n\r' || true)
if [ -z "$API_KEY" ] || [ "${#API_KEY}" -lt 32 ]; then
  fail_step "API key exchange" "Login failed; check $TMPDIR/login.txt"
fi
green "API key obtained"

# Write LP config.
mkdir -p "$TMPDIR/.aicg"
cat > "$TMPDIR/.aicg/credentials" <<YEOF
gateway_url: $GW_URL
user_id: platform_admin
api_key: $API_KEY
YEOF
cat > "$TMPDIR/.aicg/config.yaml" <<YEOF
port: $LP_PORT
log_level: info
ca_path: $ROOT/certs/ca.crt
YEOF

# --- 7. Start LP daemon -----------------------------------------------------
section "7. Start Local Proxy"

"$TMPDIR/aicg-lp" start --port $LP_PORT > "$TMPDIR/lp.log" 2>&1 &
LP_PID=$!
sleep 2

for i in $(seq 1 10); do
  if curl -s "http://127.0.0.1:$LP_PORT/_aicg/health" 2>/dev/null | grep -q ok; then
    break
  fi
  sleep 0.5
done
LP_HEALTH=$(curl -s "http://127.0.0.1:$LP_PORT/_aicg/health" 2>/dev/null || echo '{"status":"error"}')
if ! echo "$LP_HEALTH" | grep -q '"status":"ok"'; then
  fail_step "LP health" "LP did not become healthy"
fi
green "Local Proxy healthy"
dim "  $LP_HEALTH"

# --- 8. Send streaming request through LP -----------------------------------
section "8. Forward request to DeepSeek"

FORWARD_BODY=$(cat <<'JSONEOF'
{
  "model": "deepseek-v4-flash",
  "max_tokens": 50,
  "messages": [
    {"role": "user", "content": "Say hello and tell me what model you are in exactly one sentence."}
  ],
  "stream": true
}
JSONEOF
)

HTTP_CODE=$(curl -s -D "$TMPDIR/forward-headers.txt" -o "$TMPDIR/forward-response.txt" -w '%{http_code}' \
  -X POST "http://127.0.0.1:$LP_PORT/anthropic/v1/messages" \
  -H "Content-Type: application/json" \
  -H "x-api-key: lp-noop" \
  -d "$FORWARD_BODY" 2>/dev/null || echo "000")

if [ "$HTTP_CODE" -lt 200 ] || [ "$HTTP_CODE" -ge 300 ]; then
  fail_step "forward request" "HTTP $HTTP_CODE — check $TMPDIR/forward-response.txt"
fi
green "Forward returned HTTP $HTTP_CODE"

TRACE_ID=$(grep -i '^X-AICG-Trace-Id:' "$TMPDIR/forward-headers.txt" | head -1 | awk '{print $2}' | tr -d '\r\n' || true)
if [ -z "$TRACE_ID" ]; then
  fail_step "trace capture" "X-AICG-Trace-Id not found in response headers"
fi
green "Trace ID: $TRACE_ID"

# Show response excerpt.
dim "Response excerpt:"
dim "  $(head -c 200 "$TMPDIR/forward-response.txt" | tr '\n' ' ')"

# Wait for ledger write.
sleep 1

# --- 9. Show local ledger via aicg status -----------------------------------
section "9. Local ledger — aicg status"

"$TMPDIR/aicg-lp" status 2>&1 | tee "$TMPDIR/status.txt"

# --- 10. Show cost summary via aicg stats -----------------------------------
section "10. Cost summary — aicg stats"

"$TMPDIR/aicg-lp" stats 2>&1 | tee "$TMPDIR/stats.txt"

# --- 11. Show routing replay ------------------------------------------------
section "11. Routing replay — aicg routingctl"

"$TMPDIR/aicg-lp" routingctl replay "$TRACE_ID" 2>&1 | tee "$TMPDIR/routing.txt"

# --- 12. Database evidence --------------------------------------------------
section "12. Database evidence"

PG_CID=$($DOCKER_COMPOSE ps -q postgres 2>/dev/null | head -1)

db_query() {
  docker exec "$PG_CID" psql -U agentgate -d agentgate -t -c "$1" 2>/dev/null | tr -d ' ' || echo "0"
}

echo ""
bold "  audit_event count:"
docker exec "$PG_CID" psql -U agentgate -d agentgate -c \
  "SELECT trace_id::text, user_id, team_id, event_type, success, event_at FROM audit_event ORDER BY event_at DESC LIMIT 5;" 2>/dev/null

echo ""
bold "  cost_event count:"
docker exec "$PG_CID" psql -U agentgate -d agentgate -c \
  "SELECT trace_id::text, user_id, team_id, cost_cents, input_tokens, output_tokens, success FROM cost_event ORDER BY event_at DESC LIMIT 5;" 2>/dev/null

echo ""
bold "  routing_event count:"
docker exec "$PG_CID" psql -U agentgate -d agentgate -c \
  "SELECT trace_id::text, attempt_no, pool_selected, member_selected::text FROM routing_event ORDER BY event_at DESC LIMIT 5;" 2>/dev/null

echo ""
bold "  raw_record count:"
docker exec "$PG_CID" psql -U agentgate -d agentgate -c \
  "SELECT trace_id::text, event_type, metadata_type, char_length(metadata_json::text) as bytes FROM raw_record ORDER BY recorded_at DESC LIMIT 5;" 2>/dev/null

echo ""
bold "  budget_reservation count:"
docker exec "$PG_CID" psql -U agentgate -d agentgate -c \
  "SELECT team_id, trace_id::text, cents_reserved, settled FROM budget_reservation ORDER BY created_at DESC LIMIT 5;" 2>/dev/null

# --- 13. Config Hot Reload ---------------------------------------------------
section "13. Config Hot Reload"

# 13a. Record baseline route for the first request.
BASELINE_POOL=$(docker exec "$PG_CID" psql -U agentgate -d agentgate -t -c \
  "SELECT pool_selected FROM routing_event WHERE trace_id = '$TRACE_ID' ORDER BY attempt_no LIMIT 1;" 2>/dev/null | tr -d '[:space:]' || echo "unknown")
green "  Baseline pool: $BASELINE_POOL"

# 13b. Capture pre-edit reload counter so reload proof cannot race with fsnotify.
_pre_metric_val() {
  curl --cacert "$ROOT/certs/ca.crt" -sk "$GW_URL/metrics" 2>/dev/null \
    | grep 'config_reload_total' | grep '"main.yaml"' | grep '"success"' \
    | grep -oE '[0-9]+(\.[0-9]+)?$' | head -1
}
PRE_COUNT=$(_pre_metric_val)
PRE_COUNT=${PRE_COUNT:-0}

# 13c. Edit isolated policy to change default routing.
bold "  Editing policy to change on_no_match pool from standard to cheap..."
sed -i '' '/^defaults:/,/^degradation:/ s/model_pool: standard/model_pool: cheap/' "$TMPDIR/configs/policies/main.yaml"
green "  Policy updated"

# 13d. Wait for config_reload_total to increment above pre-edit baseline.
bold "  Waiting for config reload..."

RELOADED=false
for i in $(seq 1 30); do
  CURR_COUNT=$(_pre_metric_val)
  CURR_COUNT=${CURR_COUNT:-0}
  # Use awk for float comparison so fractional counters still work.
  if [ "$(awk -v c="$CURR_COUNT" -v p="$PRE_COUNT" 'BEGIN { if (c > p) print "1"; else print "0" }')" = "1" ]; then
    RELOADED=true
    break
  fi
  sleep 1
done

if [ "$RELOADED" != "true" ]; then
  fail_step "config reload" "config_reload_total{main.yaml,success} did not increment within 30s"
fi
green "  Config reload confirmed (counter $PRE_COUNT -> $CURR_COUNT)"

# 13d. Send second request through updated policy.
bold "  Sending second request..."

HTTP_CODE_2=$(curl -s -D "$TMPDIR/forward-headers-2.txt" -o "$TMPDIR/forward-response-2.txt" -w '%{http_code}' \
  -X POST "http://127.0.0.1:$LP_PORT/anthropic/v1/messages" \
  -H "Content-Type: application/json" \
  -H "x-api-key: lp-noop" \
  -d "$FORWARD_BODY" 2>/dev/null || echo "000")

if [ "$HTTP_CODE_2" -lt 200 ] || [ "$HTTP_CODE_2" -ge 300 ]; then
  fail_step "second forward request" "HTTP $HTTP_CODE_2 — check $TMPDIR/forward-response-2.txt"
fi
green "  Second request returned HTTP $HTTP_CODE_2"

TRACE_ID_2=$(grep -i '^X-AICG-Trace-Id:' "$TMPDIR/forward-headers-2.txt" | head -1 | awk '{print $2}' | tr -d '\r\n' || true)
if [ -z "$TRACE_ID_2" ]; then
  fail_step "trace capture 2" "X-AICG-Trace-Id not found in second response headers"
fi
green "  Second Trace ID: $TRACE_ID_2"

sleep 1

# 13e. Prove the route changed.
bold "  Verifying route change..."

SECOND_POOL=$(docker exec "$PG_CID" psql -U agentgate -d agentgate -t -c \
  "SELECT pool_selected FROM routing_event WHERE trace_id = '$TRACE_ID_2' ORDER BY attempt_no LIMIT 1;" 2>/dev/null | tr -d '[:space:]' || echo "unknown")

echo ""
if [ "$SECOND_POOL" != "$BASELINE_POOL" ]; then
  green "  Route changed: $BASELINE_POOL -> $SECOND_POOL"
else
  fail_step "route effect" "Route did not change (both requests routed to $BASELINE_POOL); check $TMPDIR/configs/policies/main.yaml"
fi

bold "  Second request routing evidence:"
docker exec "$PG_CID" psql -U agentgate -d agentgate -c \
  "SELECT trace_id::text, attempt_no, pool_selected, member_selected::text FROM routing_event WHERE trace_id = '$TRACE_ID_2' ORDER BY attempt_no;" 2>/dev/null

# --- 14. Summary ------------------------------------------------------------
DEMO_SUCCESS=true
section "14. Demo summary"
echo ""
green "Demo completed successfully."
echo ""
bold "  Trace ID 1 (pre-reload):  $TRACE_ID     (pool: $BASELINE_POOL)"
bold "  Trace ID 2 (post-reload): $TRACE_ID_2     (pool: $SECOND_POOL)"
bold "  GW:                       $GW_URL"
bold "  LP port:                  $LP_PORT"
echo ""
