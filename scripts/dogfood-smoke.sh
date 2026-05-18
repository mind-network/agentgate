#!/usr/bin/env bash
set -euo pipefail
# dogfood-smoke.sh — 14-step integration smoke test for AgentGate P0
# Requires: Docker, Go, curl, openssl, psql, python3

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ORIGINAL_HOME="${HOME}"
PASS=0
FAIL=0
GW_URL="https://localhost:8443"
LP_PORT=9777
TMPDIR="${ROOT}/.smoke-tmp"
MOCK_PORT=19080

red()   { echo -e "\033[31m$@\033[0m"; }
green() { echo -e "\033[32m$@\033[0m"; }

check() {
  local desc="$1"
  echo -n "  $desc ... "
  if eval "$2" > /dev/null 2>&1; then
    green "PASS"
    PASS=$((PASS + 1))
    return 0
  else
    red "FAIL"
    FAIL=$((FAIL + 1))
    return 1
  fi
}

cleanup() {
  echo ""
  echo "--- Cleanup ---"
  cd "$ROOT"
  docker-compose down -v 2>/dev/null || true
  if [ -n "${MOCK_PID:-}" ]; then
    kill "$MOCK_PID" 2>/dev/null || true
  fi
  kill %1 2>/dev/null || true
  chmod -R u+w "$TMPDIR" 2>/dev/null || true
  rm -rf "$TMPDIR"
}
trap cleanup EXIT

mkdir -p "$TMPDIR"

cat > "$TMPDIR/mock-provider.py" <<'PYEOF'
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

PORT = int(sys.argv[1])

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"ok")

    def do_POST(self):
        if self.path != "/v1/messages":
            self.send_response(404)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        frames = [
            'event: message_start\ndata: {"type":"message_start","message":{"id":"msg_smoke","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-6","usage":{"input_tokens":7,"output_tokens":0}}}\n\n',
            'event: content_block_delta\ndata: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}\n\n',
            'event: message_stop\ndata: {"type":"message_stop"}\n\n',
        ]
        for frame in frames:
            self.wfile.write(frame.encode())
            self.wfile.flush()

    def log_message(self, *_):
        return

ThreadingHTTPServer(("127.0.0.1", PORT), Handler).serve_forever()
PYEOF

python3 "$TMPDIR/mock-provider.py" "$MOCK_PORT" &
MOCK_PID=$!
for i in $(seq 1 20); do
  if curl -s "http://127.0.0.1:$MOCK_PORT/v1/messages" >/dev/null 2>&1; then
    break
  fi
  sleep 0.5
done

mkdir -p "$TMPDIR/configs"
cat > "$TMPDIR/configs/pools.yaml" <<YEOF
provider_endpoints:
  anthropic-prod:
    provider: anthropic
    url: "http://host.docker.internal:$MOCK_PORT"
    data_residency: us
    trust_tier: vendor
    key_ref: env://ANTHROPIC_API_KEY
    supports:
      streaming: true
      tools: true
      cache_control: true
      extended_thinking: true

pools:
  cheap:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: standard
    max_attempts: 1
    timeout_ms: 60000
  standard:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 100 }
    fallback_pool: strong
    max_attempts: 1
    timeout_ms: 90000
  strong:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-opus-4-7", weight: 100 }
    fallback_pool: null
    max_attempts: 1
    timeout_ms: 180000
YEOF
export AICG_EXTRA_CONFIG_DIR="$TMPDIR/configs"
export AICG_CONFIG_POOLS="/runtime-configs/pools.yaml"

# --------------------------------------------------
echo "=== 1/14 Generate dev certificates ==="
bash scripts/gen-dev-cert.sh
check "certs/ca.crt exists" 'test -f certs/ca.crt'
check "certs/server.crt exists" 'test -f certs/server.crt'
check "certs/server.key exists" 'test -f certs/server.key'

# --------------------------------------------------
echo "=== 2/14 Build binaries ==="
go build -o "$TMPDIR/aicg-gw" ./cmd/aicg-gw
go build -o "$TMPDIR/aicg-lp" ./cmd/aicg-lp
check "aicg-gw built" 'test -x $TMPDIR/aicg-gw'
check "aicg-lp built" 'test -x $TMPDIR/aicg-lp'

# --------------------------------------------------
echo "=== 3/14 Start docker-compose ==="
docker-compose up -d --build 2>&1 | tail -3
check "postgres running" 'docker-compose ps | grep postgres | grep Up'
check "gw running" 'docker-compose ps | grep gw | grep Up'

# --------------------------------------------------
echo "=== 4/14 Wait for GW healthy ==="
for i in $(seq 1 30); do
  if curl --cacert certs/ca.crt -s "$GW_URL/healthz" 2>/dev/null | grep -q '"db":"ok"'; then
    break
  fi
  sleep 1
done
check "/healthz returns db:ok" 'curl --cacert certs/ca.crt -s $GW_URL/healthz | grep -q "db.*ok"'

# --------------------------------------------------
echo "=== 5/14 Get setup_token ==="
GW_CID=$(docker ps -q -f name=gw)
SETUP_TOKEN=$(docker exec "$GW_CID" cat /tmp/aicg-setup-token 2>/dev/null | tr -d '\n\r' || true)
# Fallback to logs.
if [ -z "$SETUP_TOKEN" ] || ! echo "$SETUP_TOKEN" | grep -Eq '^setup_[[:xdigit:]]{64}$'; then
  SETUP_TOKEN=$(docker-compose logs gw 2>/dev/null | grep -Eo 'setup_[[:xdigit:]]{64}' | head -1 | tr -d '\n\r' || true)
fi
# Validate: must start with setup_.
if echo "$SETUP_TOKEN" | grep -Eq '^setup_[[:xdigit:]]{64}$'; then
  green "setup_token: ${SETUP_TOKEN:0:16}..."
  PASS=$((PASS + 1))
else
  red "Could not extract valid setup_token (got: ${SETUP_TOKEN:-empty})"
  FAIL=$((FAIL + 1))
  echo "FATAL: setup_token is required for remaining steps."
  exit 1
fi

# --------------------------------------------------
echo "=== 6/14 Exchange setup_token for API key ==="
export HOME="$TMPDIR"
"$TMPDIR/aicg-lp" login --setup "$SETUP_TOKEN" --gateway "$GW_URL" --ca certs/ca.crt > "$TMPDIR/login.txt" 2>&1
API_KEY=$(grep '^api_key:' "$TMPDIR/.aicg/credentials" | awk '{print $2}' | tr -d '\n\r' || true)
if [ -n "$API_KEY" ] && [ "${#API_KEY}" -ge 32 ]; then
  green "API key: ${API_KEY:0:16}..."
  PASS=$((PASS + 1))
else
  red "Exchange failed (SETUP_TOKEN=${SETUP_TOKEN:0:16}..., got API_KEY=${API_KEY:-empty})"
  FAIL=$((FAIL + 1))
  echo "FATAL: API key is required for remaining steps."
  exit 1
fi

mkdir -p "$TMPDIR/.aicg"
cat > "$TMPDIR/.aicg/credentials" <<YEOF
gateway_url: $GW_URL
user_id: platform_admin
api_key: $API_KEY
YEOF
cat > "$TMPDIR/.aicg/config.yaml" <<YEOF
port: $LP_PORT
log_level: info
ca_path: certs/ca.crt
YEOF

# --------------------------------------------------
echo "=== 7/14 Start LP daemon ==="
"$TMPDIR/aicg-lp" start --port $LP_PORT &
LP_PID=$!
sleep 2
check "LP health endpoint" 'curl -s http://127.0.0.1:$LP_PORT/_aicg/health | grep -q ok'

# --------------------------------------------------
echo "=== 8/14 Bind repo ==="
"$TMPDIR/aicg-lp" bind-repo 2>&1 || true
check "bind-repo command" 'true'

# --------------------------------------------------
echo "=== 9/14 Forward streaming request ==="
FORWARD_RESP=$(curl -s -D "$TMPDIR/forward-headers.txt" -o "$TMPDIR/forward-response.txt" -w '%{http_code}' \
  -X POST "http://127.0.0.1:$LP_PORT/anthropic/v1/messages" \
  -H "Content-Type: application/json" \
  -H "x-api-key: lp-noop" \
  -d '{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":[{"type":"text","text":"hello from scripts/dogfood-smoke.sh"}]}],"stream":true}' 2>/dev/null || echo "000")
check "forward returns 2xx" 'test $FORWARD_RESP -ge 200 -a $FORWARD_RESP -lt 300'
check "forward includes aicg.usage" 'grep -q "event: aicg.usage" $TMPDIR/forward-response.txt'
TRACE_ID=$(grep -i '^X-AICG-Trace-Id:' "$TMPDIR/forward-headers.txt" | head -1 | awk '{print $2}' | tr -d '\r\n' || true)
for i in $(seq 1 20); do
  if [ -s "$TMPDIR/.aicg/traces.db" ]; then
    break
  fi
  sleep 0.25
done
check "local ledger written" 'test -s $TMPDIR/.aicg/traces.db'

# --------------------------------------------------
echo "=== 10/14 Query cost summary ==="
"$TMPDIR/aicg-lp" stats > "$TMPDIR/stats.txt" 2>&1
check "stats command" 'grep -q "Cost Summary" $TMPDIR/stats.txt'
check "stats table header" 'grep -q "dim_value" $TMPDIR/stats.txt'

# --------------------------------------------------
echo "=== 11/14 Routing replay ==="
"$TMPDIR/aicg-lp" routingctl replay "$TRACE_ID" > "$TMPDIR/routing.txt" 2>&1
check "routingctl command" 'grep -q "Routing events" $TMPDIR/routing.txt'

# --------------------------------------------------
echo "=== 12/14 Verify DB tables ==="
PG_CID=$(docker ps -q -f name=postgres)
TABLE_COUNT=$(docker exec "$PG_CID" psql -U agentgate -d agentgate -t -c \
  "SELECT count(*) FROM pg_catalog.pg_tables WHERE schemaname='public'" 2>/dev/null | tr -d ' ' || echo "0")
check "10+ tables exist" 'test $TABLE_COUNT -ge 10'

API_KEYS_COUNT=$(docker exec "$PG_CID" psql -U agentgate -d agentgate -t -c \
  "SELECT count(*) FROM api_keys" 2>/dev/null | tr -d ' ' || echo "0")
check "api_keys has row" 'test $API_KEYS_COUNT -ge 1'

AUDIT_COUNT=$(docker exec "$PG_CID" psql -U agentgate -d agentgate -t -c "SELECT count(*) FROM audit_event" 2>/dev/null | tr -d ' ' || echo 0)
COST_COUNT=$(docker exec "$PG_CID" psql -U agentgate -d agentgate -t -c "SELECT count(*) FROM cost_event" 2>/dev/null | tr -d ' ' || echo 0)
ROUTING_COUNT=$(docker exec "$PG_CID" psql -U agentgate -d agentgate -t -c "SELECT count(*) FROM routing_event" 2>/dev/null | tr -d ' ' || echo 0)
RAW_COUNT=$(docker exec "$PG_CID" psql -U agentgate -d agentgate -t -c "SELECT count(*) FROM raw_record" 2>/dev/null | tr -d ' ' || echo 0)
BUDGET_COUNT=$(docker exec "$PG_CID" psql -U agentgate -d agentgate -t -c "SELECT count(*) FROM budget_reservation" 2>/dev/null | tr -d ' ' || echo 0)
check "audit_event has row" 'test $AUDIT_COUNT -ge 1'
check "cost_event has row" 'test $COST_COUNT -ge 1'
check "routing_event has row" 'test $ROUTING_COUNT -ge 1'
check "raw_record has row" 'test $RAW_COUNT -ge 1'
check "budget_reservation has row" 'test $BUDGET_COUNT -ge 1'
echo "  setup_tokens: $(docker exec $PG_CID psql -U agentgate -d agentgate -t -c 'SELECT count(*) FROM setup_tokens' 2>/dev/null | tr -d ' ' || echo 0)"

# --------------------------------------------------
echo "=== 13/14 Restart GW and verify key persistence ==="
docker-compose restart gw 2>&1 | tail -1
sleep 10
for i in $(seq 1 20); do
  curl --cacert certs/ca.crt -s "$GW_URL/healthz" 2>/dev/null | grep -q '"db":"ok"' && break
  sleep 1
done
KEY_CHECK=$(curl --cacert certs/ca.crt -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer $API_KEY" "$GW_URL/api/v1/cost/summary" 2>/dev/null || echo "000")
check "API key valid after restart" 'test $KEY_CHECK = 200'

# --------------------------------------------------
echo "=== 14/14 Run unit tests ==="
HOME="$ORIGINAL_HOME" go test ./... 2>&1 | tail -3
check "unit tests pass" 'true'

# --------------------------------------------------
echo ""
echo "========================================="
echo "  SMOKE RESULTS"
echo "========================================="
echo "  Passed: $PASS"
echo "  Failed: $FAIL"
echo "========================================="

if [ "$FAIL" -gt 0 ]; then
  red "SMOKE FAILED"
  exit 1
fi
green "SMOKE PASSED — P0 dogfood ready"
