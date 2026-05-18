# AgentGate Phase 0 Demo Plan

> Version: v0.1 | Date: 2026-05-11
> Audience: internal technical review, platform engineering, SRE
> Companion docs: `docs/dogfood-runbook.md`, `docs/whitepaper.md`, `docs/operations/operator-guide.md`
> Companion docs (team dogfood): [`./dogfood-onboarding.md`](./dogfood-onboarding.md) (D-1 / D-Day / Week-1 timeline)

## 1. Demo positioning

The goal of the Phase 0 demo is not to show a complete control-plane product. It is to prove that AgentGate already has the core closed loop required to dogfood:

- Developer requests flow from the local LP into the enterprise GW, then on to the upstream provider.
- The GW carries out identity, policy, routing, cost, audit, raw-metadata, and budget-related bookkeeping along the request path.
- Key evidence can be stitched together through `trace_id`, and verified via the CLI and the database.
- Configuration changes hot-reload, and bad configuration does not break the previous version.

Recommended demo style: **CLI + guided demo script first; do not introduce a formal Web UI in P0.**

Reasoning:

- The current P0 product positioning is cost observability + internal dogfood; the formal dashboard is scoped to P1+ in the architecture.
- The GW already exposes read-only JSON APIs: `/api/v1/cost/summary` and `/api/v1/routing/events?trace_id=...`, which the CLI can reuse directly.
- Technical reviewers care more about end-to-end evidence, persistence, and operability than about a half-finished UI — terminal output is more credible than a stub page.
- Avoid pulling in Next.js, embedded static assets, page state, and RBAC UI scope, which would turn the P0 demo into a P1 dashboard project.

## 2. Demo storyline

### 2.1 Narrative structure

Lay it out as "developer view → platform view → operations view".

| Segment | What we show | What it proves |
|---|---|---|
| Developer onboarding | `aicg login`, `aicg start`, curl / Claude Code request through LP | The onboarding surface is light; no changes required in the coding agent |
| Gateway governance | trace_id, policy route, routing_event, cost_event, audit_event | The GW actually governs the request path — it is not a passthrough proxy |
| Cost visibility | `aicg stats`, `/api/v1/cost/summary` | The platform team can see cost broken down by team / user |
| Routing explainability | `aicg routingctl replay <trace_id>` | Routing decisions can be replayed for incident / debug |
| Operations loop | Hot reload, metrics, old config preserved on failure | Config releases do not require rebuilding images or restarting the GW |

### 2.2 Recommended demo script

Add a new demo script:

```bash
scripts/demo-p0.sh
```

It can reuse `scripts/dogfood-smoke.sh` plumbing, but the output should be aimed at humans watching the screen rather than at PASS/FAIL acceptance.

Recommended steps:

1. Generate dev TLS certs: `bash scripts/gen-dev-cert.sh`.
2. Build `aicg-gw` and `aicg-lp`.
3. Start the real DeepSeek Anthropic-compatible upstream, authenticated via `DEEPSEEK_API_KEY`; if the key is missing, fail at the preflight stage.
4. Start Postgres + GW.
5. Wait for `GET /healthz` to return `db: ok`.
6. Read the setup token and exchange it for a real API key with `aicg login --setup ...`.
7. Start the LP: `aicg start --port 9777`.
8. Send an Anthropic streaming request to the LP.
9. Print the `X-AICG-Trace-Id` from the response.
10. Show the LP local ledger: `aicg status`.
11. Show the cost summary: `aicg stats`.
12. Show the routing replay: `aicg routingctl replay <trace_id>`.
13. Show DB row counts in `audit_event`, `cost_event`, `routing_event`, `raw_record`, `budget_reservation`.
14. Edit a temporary policy/config, show `config_reload_total{result="success"}` increment.
15. Write an invalid policy, show `config_reload_total{result="parse_error"}` increment, with the previous config still serving traffic.

The demo script should clean up temporary resources by default; on failure, preserve the log paths needed for on-the-spot debugging.

## 3. Supporting capabilities to add

### 3.1 Required: demo script

Add `scripts/demo-p0.sh`.

Requirements:

- Uses the real DeepSeek Anthropic-compatible upstream and requires `DEEPSEEK_API_KEY`; the preflight fails if the key is missing.
- Output uses section headers, e.g. `1. Bootstrap gateway`, `2. Forward request`, `3. Trace evidence`.
- Each section shows the key command and key result — no large JSON dumps.
- Captures `trace_id` automatically; later commands all reference the same trace.
- On failure, says clearly which step stopped and where the corresponding log lives.

### 3.2 Required: enhance `aicg status`

Today, `aicg status` only proves the local ledger can be opened. For the demo, it should also surface a recent-trace summary.

Suggested output:

```text
Local Proxy
  Ledger: ~/.aicg/traces.db

Recent traces
  trace_id                              model                 provider    tokens    cost
  0190...                              claude-sonnet-4-6     anthropic   7/3       0 cents
```

Minimal implementation:

- Add `ListRecent(limit int)` to `internal/lp/ledger`.
- `aicg status` shows the most recent 5 entries by default.
- When there are no traces, print `(no local traces yet)`.

### 3.3 Recommended: polish `aicg stats`

`aicg stats` already calls the GW cost summary, but the output skews toward debug. Change it to a stable table that is suitable for projecting on screen.

Suggested output:

```text
Cost Summary
  bucket       user              requests   tokens   cost
  dogfood      platform_admin    1          10       0 cents
```

Note: known P0 cost-fidelity limitations still apply. When provider usage is not fully propagated to `cost_event.cost_cents`, the demo can only show that "a cost record was produced" and that "the stats endpoint is readable" — do not claim the amount is exact.

### 3.4 Recommended: polish `routingctl replay`

The current replay output is sufficient, but the demo can be more legible.

Suggested output:

```text
Routing Replay
  trace_id: 0190...
  attempt:  1
  pool:     standard
  member:   anthropic-prod / claude-sonnet-4-6
```

If `member` is a JSON string, the CLI should make a best-effort attempt to extract `endpoint_id` and `model`; if parsing fails, fall back to the raw JSON.

### 3.5 Do not build a formal Web UI in P0

Do not add a Next.js dashboard or embedded static page in P0.

Acceptable lightweight alternatives:

- Provide two JSON API curls in the demo doc:
  - `GET /api/v1/cost/summary`
  - `GET /api/v1/routing/events?trace_id=<trace_id>`
- If you really need something "UI-like", prefer terminal tables over a browser page.

Out of scope:

- No approval-queue page, because `block`, `redact`, and `require_approval` are P1+.
- No raw prompt/response viewer, because P0 is `metadata_only` — raw bodies are not stored.
- No multi-tenant admin console, because the P0 user/repo YAMLs are still mostly reload-only or basic identification.

## 4. On-site demo flow

### 4.1 Opening

Suggested opening:

```text
This demo shows the Phase 0 dogfood-ready baseline: AgentGate already routes coding-agent requests through a local proxy into the enterprise gateway, leaving behind verifiable cost, routing, audit, and metadata evidence. P0 does not demonstrate approval, blocking, redaction, or a formal dashboard — those are P1+ governance enhancements.
```

### 4.2 Run the main demo

Recommended command:

```bash
bash scripts/demo-p0.sh
```

The script should pause at a few key evidence points:

- GW health: `{"status":"ok","components":{"db":"ok"}}`
- LP health: `{"status":"ok"}`
- Request trace: `X-AICG-Trace-Id: ...`
- Local ledger: same trace appears.
- Cost summary: at least one request appears.
- Routing replay: same trace shows pool / member.
- DB evidence: all core tables have rows.
- Reload evidence: `config_reload_total` increments.

### 4.3 Manual drill-down

If reviewers push for more, drill down by hand:

```bash
curl --cacert certs/ca.crt \
  -H "Authorization: Bearer $API_KEY" \
  https://localhost:8443/api/v1/cost/summary

curl --cacert certs/ca.crt \
  -H "Authorization: Bearer $API_KEY" \
  "https://localhost:8443/api/v1/routing/events?trace_id=$TRACE_ID"

docker exec "$PG_CID" psql -U agentgate -d agentgate -c \
  "select trace_id, user_id, team_id, cost_cents, success from cost_event order by event_at desc limit 5;"
```

### 4.4 Wrap-up

Suggested closing emphasis:

- P0 has proven the core path and the evidence path.
- P0's primary value is internal dogfood, cost visibility, routing explainability, and operability.
- P1 brings hard governance: block / redact / approval, fallback / circuit breaker, formal dashboard, webhook, TimescaleDB, etc.

## 5. Acceptance criteria

Once the supporting capabilities land, the demo should meet:

- `scripts/demo-p0.sh` runs end-to-end on a clean checkout with `DEEPSEEK_API_KEY` set; if the key is missing, the script exits at preflight with a clear prompt.
- During the demo, we can capture a stable `trace_id` and use it to stitch together the LP ledger, the GW routing API, and the database records.
- `aicg status` shows recent traces, not just "ledger is openable".
- `aicg stats` output is suitable for projecting.
- `aicg routingctl replay <trace_id>` output is suitable for projecting.
- The demo doc does not promise capabilities that P0 does not have: approval, blocking, redaction, raw-body inspection, formal dashboard.

## 6. Risks and boundaries

| Risk | Mitigation |
|---|---|
| `DEEPSEEK_API_KEY` missing on site | Script fails early at preflight with a setup pointer; no silent fallback to a mock provider |
| Cost numbers are 0 or imprecise | Call out the known P0 limitation: cost fidelity is a separate follow-up |
| Audience expects a Web UI | Explain that P0 presents governance evidence via CLI / JSON APIs; the formal dashboard is P1+ |
| Demo script output too verbose | Default to printing only the key evidence; full logs go to a temporary directory |
| Config reload mistaken for "all identity changes apply instantly" | Distinguish live-effect vs reload-only files per `config-reload.md` |

## 7. Suggested implementation split

If we later turn this document into code changes, split it as a small handoff:

1. Add `scripts/demo-p0.sh` using the real DeepSeek Anthropic-compatible upstream and environment-isolation logic.
2. Enhance the ledger query and `aicg status` output.
3. Polish the table output for `aicg stats` and `routingctl replay`.
4. Add focused tests, and run `make test`, `make build`, `make lint`.

Do not split out a UI handoff. Design the page, permissions, and static-asset embedding only when the Phase 1 dashboard work actually starts.
