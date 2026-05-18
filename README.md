# AgentGate

> Enterprise gateway for AI coding agents — policy, routing, cost attribution, and audit for Claude Code / OpenAI-compatible traffic.

AgentGate is a CCR-style AI Coding gateway that puts a thin **Local Proxy (LP)** in front of tools like Claude Code, Cursor, and Aider, and routes their traffic through an enterprise **Gateway (GW)**. The gateway carries out policy checks, model-pool routing, cost attribution, and audit before forwarding to upstream providers (Anthropic, OpenAI-compatible endpoints, Bedrock, Azure, etc.). LP-side onboarding is transparent — install, log in once, set the environment variables, and existing coding agents work unchanged.

## Status

- **Phase 0 (current)**: dogfood baseline. LP → GW → provider closed loop with policy/routing/cost/audit/raw-metadata evidence in Postgres. Single-binary distribution (`aicg-gw`, `aicg-lp`), Docker Compose deployment, no dashboard UI yet.
- **Phase 1+**: formal dashboard, approval queue, block/redact, fallback/circuit breaker, TimescaleDB, webhook integration.
- The authoritative specification is [`docs/architecture/SYSTEM-DESIGN.md`](docs/architecture/SYSTEM-DESIGN.md) (v0.2.3). Decisions §1 and the Implementation Readiness Checklist §20 (R-1 ~ R-22) are the P0 baseline.

## Architecture at a glance

```
Developer Workstation                Enterprise VPC
┌─────────────────┐      ┌──────────────────────────────────┐
│ coding agent    │      │  aicg-gw                         │
│ (Claude Code,   │      │  ├─ Edge (rate-limit, trace_id) │
│  Cursor, ...)   │      │  ├─ Auth + Policy (YAML + CEL)  │
└────────┬────────┘      │  ├─ Routing (pools + fallback)  │
         │ loopback      │  ├─ Provider Adapters            │
┌────────▼────────┐ mTLS │  ├─ Budget / Cost / Audit        │
│ aicg-lp         │─────▶│  └─ Raw store (metadata-only P0) │
│ port 7777       │      │             │                    │
└─────────────────┘      │      ┌──────┴──────┐             │
                         │      │ Postgres 16 │             │
                         │      └─────────────┘             │
                         └──────────────────────────────────┘
                                       │
                                       ▼
                         Anthropic / OpenAI-compatible / Bedrock / Azure
```

## Quick start (single-host, dev/staging)

For a clean-checkout, single-user smoke test, see [`docs/dogfood-runbook.md`](docs/dogfood-runbook.md):

```bash
bash scripts/gen-dev-cert.sh             # 1. dev TLS certs
make build                               # 2. build aicg-gw + aicg-lp
export ANTHROPIC_API_KEY=sk-ant-...
docker-compose up -d                     # 3. Postgres + GW

# 4. exchange the bootstrap setup token for an admin API key
docker-compose logs gw | grep "AGENTGATE BOOTSTRAP" -A 5
aicg login --setup <token> --gateway https://localhost:8443 --ca certs/ca.crt

aicg start --port 7777 &                 # 5. start LP

curl -s http://127.0.0.1:7777/anthropic/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: lp-noop" \
  -d '{"model":"claude-sonnet-4-6","max_tokens":10,
       "messages":[{"role":"user","content":"hello"}],"stream":true}'
```

For a team dogfood timeline (D-1 admin / D-Day developer / Week-1 retro), see [`docs/operations/dogfood-onboarding.md`](docs/operations/dogfood-onboarding.md).

## Repository layout

```
cmd/aicg-gw/             # gateway entry point
cmd/aicg-lp/             # local proxy entry point
internal/shared/         # GW + LP shared: envelope / ir / transformer / scan / wire / secretref
internal/lp/             # LP only: server / tagger / session / repobinder / gwclient / cli
internal/gw/             # GW only: edge / auth / policy / routing / provider / budget / cost / audit / rawstore / dashboard / db
configs/                 # YAML config (policies / pools / budgets / identity / pricing)
docs/architecture/       # authoritative architecture documents
docs/operations/         # operator-facing guides (user, operator, onboarding, demo plan)
scripts/                 # dev / smoke / demo / cert generation
tests/                   # integration + conformance + fixtures
```

## Build, test, lint

```bash
make build      # produces ./aicg-gw and ./aicg-lp
make test       # go test ./...
make lint       # golangci-lint
```

## Documentation index

| Document | Audience | Contents |
|---|---|---|
| [`docs/architecture/SYSTEM-DESIGN.md`](docs/architecture/SYSTEM-DESIGN.md) | Architecture / security review | Authoritative system design (v0.2.3) — decision log, request lifecycle, policy/routing/budget/storage/audit, Implementation Readiness Checklist |
| [`docs/architecture/policy-cookbook.md`](docs/architecture/policy-cookbook.md) | platform_admin, security | Policy YAML + CEL recipes, priority bands, anti-patterns, rollout checklist |
| [`docs/architecture/threat-model.md`](docs/architecture/threat-model.md) | Security review | STRIDE per-trust-boundary, Top-N threat list, residual risk acceptance |
| [`docs/architecture/CHANGELOG.md`](docs/architecture/CHANGELOG.md) | All | Architecture revision history |
| [`docs/dogfood-runbook.md`](docs/dogfood-runbook.md) | Single user | Zero-to-first-request deployment |
| [`docs/operations/user-guide.md`](docs/operations/user-guide.md) | Developer | LP install, login, env setup, troubleshooting |
| [`docs/operations/operator-guide.md`](docs/operations/operator-guide.md) | platform_admin, SRE | Deploy, configure, monitor, incident response, upgrade |
| [`docs/operations/dogfood-onboarding.md`](docs/operations/dogfood-onboarding.md) | platform_admin + team | D-1 / D-Day / Week-1 dogfood timeline |
| [`docs/operations/phase-0-demo-plan.md`](docs/operations/phase-0-demo-plan.md) | Technical review | Phase 0 demo narrative + supporting tooling plan |

## Tech stack (planned)

| Layer | Choice | Notes |
|---|---|---|
| GW / LP language | Go 1.25 | Single binary; standard `net/http` + chi/v5 |
| Policy expression | cel-go | YAML + CEL, three-slot Decision (PrimaryAction / Modifiers / SideEffects) |
| Database | Postgres 16 (P0); TimescaleDB extension from P1 | pgx/v5 |
| Config hot reload | fsnotify + atomic double-buffer | inotify reload; last valid config kept on failure |
| Provider SDK | Anthropic / OpenAI official; others via OpenAI-compatible HTTP | |
| Encryption | KMS envelope (P2+); secret resolver and raw-store KMS are independent subsystems | |
| Dashboard | Next.js 14 + Tailwind + shadcn/ui (P1+) | embedded via `embed.FS` in the GW binary |
| Tests | `go test` + testcontainers-go (Postgres) | |
| Container | Distroless static | |
