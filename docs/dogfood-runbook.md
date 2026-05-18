# AgentGate Dogfood Runbook (P0)

> Team day-one onboarding (D-1 admin / D-Day developer / Week-1 retro) — start with [`operations/dogfood-onboarding.md`](operations/dogfood-onboarding.md). This document covers the single-user clean-checkout deployment path.

Step-by-step guide to deploy AgentGate from a clean checkout and run the first
forwarded request.

## Prerequisites

- Docker 24+ (for Postgres)
- Go 1.25 (for building binaries)
- openssl (for TLS certs)
- curl, psql (for verification)

## Step 1: Generate dev certificates

```bash
bash scripts/gen-dev-cert.sh
```

Creates `certs/ca.crt`, `certs/server.crt`, `certs/server.key`.

The script also reads a repo-local `.env` file when present. Supported items:

```bash
AICG_PUBLIC_BASE_URL=https://gw.example.com:8443
AICG_TLS_DOMAIN=gw.example.com
```

## Step 2: Build binaries

```bash
make build
```

Produces `./aicg-gw` and `./aicg-lp`. Rebuild after pulling source changes; a stale binary may produce output that does not match the current documented behavior.

## Step 3: Start services

```bash
export ANTHROPIC_API_KEY=sk-ant-...
docker-compose up -d
```

Postgres starts first. GW container retries for up to 30s until Postgres is
ready, then runs migrations automatically.

Verify:

```bash
curl --cacert certs/ca.crt https://localhost:8443/healthz
# {"status":"ok","components":{"db":"ok"}}
```

## Step 4: Get setup_token

```bash
docker-compose logs gw | grep "AGENTGATE BOOTSTRAP" -A 5
```

Copy the `setup_...` token printed at first boot.

If the token file was written to disk:

```bash
cat /var/lib/aicg/aicg-setup-token
```

## Step 5: Login from LP

```bash
aicg login --setup <token> --gateway https://localhost:8443 --ca certs/ca.crt
```

Current LP CLI parsing expects space-separated flag values; do not use
`--setup=<token>` / `--gateway=<url>` form in copy-paste commands.

Verifies credentials saved:

```bash
cat ~/.aicg/credentials
# gateway_url: https://localhost:8443
# user_id: platform_admin
# api_key: <64-char-hex>
```

## Step 6: Start LP daemon

```bash
aicg start --port 7777 &
curl http://127.0.0.1:7777/_aicg/health
# {"status":"ok"}
```

## Step 7: Forward a request

```bash
curl -s http://127.0.0.1:7777/anthropic/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: lp-noop" \
  -d '{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":"hello"}],"stream":true}'
```

Check the gateway logs for the trace_id and `aicg.usage` injection.

## Verification Checklist

- [ ] `curl https://localhost:8443/healthz` returns 200 with `db: ok`
- [ ] `aicg login` saves real API key (64 hex chars), NOT the setup_token
- [ ] `aicg start` listens on 127.0.0.1:7777
- [ ] `curl /_aicg/health` returns 200
- [ ] Forward request produces SSE stream with `aicg.usage` event
- [ ] `psql` shows rows in `audit_event`, `cost_event`, `routing_event`, `raw_record`, `api_keys`, `budget_reservation`
- [ ] GW restart (`docker-compose restart gw`) preserves minted API key
- [ ] `aicg stats` returns data (not "not yet wired")
- [ ] `aicg routingctl replay <trace_id>` outputs chain

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| GW exits with `secret_ref unresolved` | Set `ANTHROPIC_API_KEY` env var or remove `key_ref` from pools.yaml |
| `curl: SSL certificate problem` | Use `--cacert certs/ca.crt` |
| Postgres connection refused | Wait 30s for GW retry; check `docker ps` |
| `aicg login` fails with 401 | Ensure Authorization header is `Bearer <token>` |

> Next step: team onboarding → [`operations/dogfood-onboarding.md`](operations/dogfood-onboarding.md) (D-1 admin / D-Day developer / Week-1 retro timeline).
