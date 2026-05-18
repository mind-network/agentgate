# AgentGate Operator Guide

> Version: v0.2.3 | Date: 2026-05-10
> Audience: platform_admin, AI Platform team, SRE
> Companion docs: SYSTEM-DESIGN.md, dogfood-runbook.md, policy-cookbook.md, config-reload.md

## 1. System overview

AgentGate Gateway (GW) is the enterprise-side AI Coding traffic governance gateway. Deployment topology:

```
Developer Workstation                    Enterprise VPC
┌─────────────────┐         ┌──────────────────────────────────┐
│  aicg (LP)      │ ──────▶ │  GW (aicg-gw)                    │
│  port 7777       │  mTLS   │  ├─ Edge (rate-limit, trace_id) │
└─────────────────┘         │  ├─ Ingress Pipeline             │
                            │  ├─ Policy Engine (YAML + CEL)  │
                            │  ├─ Routing Engine               │
                            │  ├─ Provider Adapters            │
                            │  ├─ Budget / Cost                │
                            │  └─ Audit / Raw Store            │
                            │       │                          │
                            │  ┌────┴────────┐                 │
                            │  │  Postgres 16 │                │
                            │  └─────────────┘                 │
                            └──────────────────────────────────┘
```

**Physical components:**

| Component | Form factor | Deployment |
|---|---|---|
| `aicg-gw` | Single binary (includes Dashboard static assets) | Docker Compose / Helm / bare binary |
| Postgres 16 | External database | Docker Compose (dev) / managed (prod) |
| Config directory | YAML files | bind mount or git-ops sync |

## 2. Deployment

### 2.1 Quick start (Docker Compose, dev/staging)

Follow steps 1–3 of `dogfood-runbook.md`:

```bash
# 1. Generate TLS certs
bash scripts/gen-dev-cert.sh

# 2. Build
make build

# 3. Set provider key and start
export ANTHROPIC_API_KEY=sk-ant-...
docker-compose up -d
```

### 2.2 Production deployment checklist

Confirm each item before deploying:

- [ ] Postgres 16 instance is provisioned and reachable (`psql -h <host> -U aicg -d agentgate`)
- [ ] TLS certs signed by the enterprise CA (`certs/server.crt`, `certs/server.key`)
- [ ] All config YAMLs customized for the enterprise environment (see §3)
- [ ] Provider API keys configured (`env://` or `file://`, see §4)
- [ ] Firewall / security group allows GW → Postgres (5432)
- [ ] Firewall allows GW → upstream provider API (443)
- [ ] `AICG_SETUP_TOKEN` available for first bootstrap (printed to logs only on GW startup)
- [ ] Metrics scrape endpoint `GET /metrics` wired into Prometheus
- [ ] Log aggregation configured (stdout JSON → Fluentd / Loki, etc.)

### 2.3 Bare-binary deployment

```bash
# Directory layout
mkdir -p /etc/agentgate/policies /etc/agentgate/identity /run/agentgate/secrets

# Place config files (see §3)
cp configs/pools.yaml configs/pricing.yaml /etc/agentgate/
cp configs/policies/main.yaml /etc/agentgate/policies/
cp configs/identity/users.yaml configs/identity/teams.yaml configs/identity/repos.yaml /etc/agentgate/identity/

# Start
AICG_SETUP_TOKEN=$(openssl rand -hex 32) \
  ANTHROPIC_API_KEY=sk-ant-... \
  aicg-gw
```

### 2.4 Runtime health checks

```bash
# Health
curl -sk https://localhost:8443/healthz
# {"status":"ok","components":{"db":"ok"}}

# Version
curl -sk https://localhost:8443/_aicg/version
# {"version":"0.3.1","commit":"0e7a2b1","build_time":"2026-05-10T08:00:00Z"}

# Metrics
curl -sk https://localhost:8443/metrics | head -20
```

### 2.5 First-time bootstrap

The GW emits a one-time setup token on first start:

```bash
# Docker Compose
docker-compose logs gw | grep "AGENTGATE BOOTSTRAP" -A 5

# Bare binary
journalctl -u aicg-gw | grep "AGENTGATE BOOTSTRAP" -A 5
```

Example output:

```
AGENTGATE BOOTSTRAP
-------------------
  Setup token: setup_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx
  This token is valid for ONE use and expires in 60 minutes.
  Use: aicg login --setup <token> --gateway <gw-url>
-------------------
```

The current LP CLI parser only accepts space-separated flag values; do not use the `--setup=<token>` / `--gateway=<url>` form when copy-pasting commands.

The first setup login creates an API key with the `platform_admin` role. Subsequent users are created by the admin (see §6.2).

> Next up: dogfood team day-one onboarding → [`dogfood-onboarding.md`](./dogfood-onboarding.md) (D-1 admin prep checklist + credential distribution).

## 3. Configuration file management

### 3.1 Configuration file map

| File | Container path | Responsibility | P0 effective |
|---|---|---|---|
| `pools.yaml` | `/configs/pools.yaml` | Provider endpoint registry + model pool definitions | ✅ |
| `pricing.yaml` | `/configs/pricing.yaml` | Model pricing table | ✅ |
| `policies/main.yaml` | `/configs/policies/main.yaml` | Policy rules | ✅ |
| `identity/teams.yaml` | `/configs/identity/teams.yaml` | Teams and budgets | ✅ |
| `identity/users.yaml` | `/configs/identity/users.yaml` | Users and roles | reload-only |
| `identity/repos.yaml` | `/configs/identity/repos.yaml` | Repo registry | reload-only |

> **Hot reload**: all files auto-reload via inotify — no GW restart needed. See `config-reload.md`.

### 3.2 Edit workflow

1. **Local validation**: `aicg policyctl validate` (parse + CEL compile + match simulation)
2. **Edit the file**: modify the target YAML
3. **Observe the reload**: `curl -sk https://localhost:8443/metrics | grep config_reload_total`
4. **Verify the effect**: send a test request and confirm the policy matches
5. **On error**: GW keeps the last valid configuration; once the YAML is fixed it auto-reloads

### 3.3 Sensitive file protection

The following files require dual PR approval (process control):

- `configs/pools.yaml` — adding / modifying endpoints affects all traffic routing
- `configs/policies/*.yaml` — policy rule changes
- `configs/pricing.yaml` — pricing-table changes
- `configs/identity/users.yaml`, `configs/identity/teams.yaml` — identity-mapping changes

## 4. Secret management

### 4.1 Provider API key configuration

P0 supports two schemes: `env://` and `file://`.

**env:// scheme (Docker Compose):**

```yaml
# pools.yaml
provider_endpoints:
  anthropic-prod:
    key_ref: "env://ANTHROPIC_API_KEY"
```

```bash
# docker-compose.yaml
environment:
  - ANTHROPIC_API_KEY=sk-ant-...
```

**file:// scheme (recommended for production):**

```bash
# Create secret file (mode 0600)
echo -n "sk-ant-my-key" > ./configs/secrets/anthropic-prod
chmod 600 ./configs/secrets/anthropic-prod
```

```yaml
# pools.yaml
provider_endpoints:
  anthropic-prod:
    key_ref: "file:///run/agentgate/secrets/anthropic-prod"
```

> Residual P0 risk: provider keys reside in GW process memory in cleartext. See SYSTEM-DESIGN §10.x for mitigations.

### 4.2 Secret rotation

```bash
# Just write the new value — GW auto-detects and re-resolves
echo -n "sk-ant-new-key" > ./configs/secrets/anthropic-prod

# Verify
curl -sk https://localhost:8443/metrics | grep secret_reload_total
# secret_reload_total{ref="file",result="success"} 2
```

See `config-reload.md` §Secret Rotation for the full procedure.

## 5. Monitoring and alerting

### 5.1 Key metrics

| Metric | Meaning | Suggested alert threshold |
|---|---|---|
| `config_reload_total` | Config reload count (success / failure) | `result="parse_error"` > 0 alerts immediately |
| `secret_reload_total` | Secret reload count | `result="resolve_error"` > 0 alerts immediately |
| `request_total` | Total requests (by status_code, provider) | — |
| `request_duration_seconds` | Request latency distribution | p95 > 30s alerts |
| `upstream_errors_total` | Upstream provider errors | 5xx rate > 10% alerts |
| `circuit_breaker_state` | Breaker state (per provider) | `state="open"` alerts |
| `budget_reservation_total` | Budget reservation stats | — |
| `policy_decisions_total` | Policy decisions (by action) | `action="block"` spike 10× alerts |

### 5.2 Health checks

```bash
# Simple liveness probe
curl -sk https://localhost:8443/healthz

# With component breakdown
curl -sk https://localhost:8443/healthz?full=true
# {"status":"ok","components":{"db":"ok","config":"ok","secrets":"ok"}}
```

### 5.3 Logs

GW emits structured JSON logs to stdout:

```json
{
  "level": "info",
  "ts": "2026-05-10T09:30:00Z",
  "msg": "request completed",
  "event": "request",
  "trace_id": "01hxxxx...",
  "user_id": "alice",
  "team_id": "acme-eng",
  "status": 200,
  "cost_cents": 42,
  "duration_ms": 8230,
  "routed_to": "anthropic-prod:claude-sonnet-4-6"
}
```

Key event types:

| event | Meaning |
|---|---|
| `request` | Request completed (with cost, routed_to) |
| `config_reload` | Config reload |
| `secret_reload` | Secret reload |
| `circuit_breaker` | Breaker state change |
| `budget_reservation` | Budget reservation action |
| `policy_decision` | Policy decision |
| `raw_store_write` | Raw store write (P2+) |

## 6. User and team management

### 6.1 Roles

| Role | Permissions |
|---|---|
| `developer` | Normal use; can view own usage; can view own raw (P2+) |
| `team_admin` | View team usage + members' raw (redacted); manage team members |
| `platform_admin` | All permissions: configuration, policies, user management, full raw access |

### 6.2 User management

**Add a user:**

1. Edit `configs/identity/users.yaml`:
   ```yaml
   users:
     - user_id: alice
       email: alice@example.com
       role: developer
       team_id: acme-eng
   ```
2. Generate an invite link and send it to the user:
   ```
   aicg admin invite --role developer --team <team-id> --user <user-id>
   aicg login --invite <invite-token> --gateway <gw-url>
   ```
   The first command calls GW `POST /api/v1/admin/invites` to mint a one-shot invite token; the second is the login command for the user. As above, the current CLI uses space-separated flag values.

**Disable a user:**

1. Edit `configs/identity/users.yaml`, add `disabled: true`
2. The user's API key fails on the next request (returns 401)

### 6.3 Team management

Edit `configs/identity/teams.yaml`:

```yaml
teams:
  - team_id: acme-eng
    name: "Engineering"
    budget:
      monthly_cap_cents: 50000       # $500/month
    tags: ["eu-data-residency"]
```

### 6.4 Repo registration

Edit `configs/identity/repos.yaml`:

```yaml
repos:
  - repo_id: repo_acme_payments
    remote_url: "https://github.com/acme/payments"
    owners: ["acme-eng"]
    sensitivity: high
    tags: ["restricted", "pci"]
```

Repos with `sensitivity: high` automatically trigger `route_to_private_model` and only route to the private-domain endpoint.

## 7. Policy management

### 7.1 Policy lifecycle

```
Write YAML + CEL rules
  ↓
Local validation: aicg policyctl validate
  ↓
PR to git → CI lint → fixture replay
  ↓
Merge → inotify hot reload takes effect
  ↓
30-minute observation window: roll back immediately on anomaly
```

### 7.2 Policy validation

```bash
# Syntax + CEL compile + match simulation
aicg policyctl validate --file policies/main.yaml

# Replay against fixtures
aicg policyctl validate --file policies/main.yaml --fixtures fixtures/policy/*.json

# dry-run: take the most recent 100 real requests, diff old vs. new policy
aicg policyctl dry-run --window 100
```

### 7.3 Rollback

```bash
# Quick rollback: git revert + push, GW auto inotify reload
git revert <bad-commit>
git push

# Or swap the file in place
cp policies/main.yaml.backup configs/policies/main.yaml

# Verify the rollback succeeded
curl -sk https://localhost:8443/metrics | grep config_reload_total
# config_reload_total{file="main.yaml",result="success"} N+1
```

Rollback thresholds:
- 5xx rate > 0.5%
- `block` hit rate spikes 10×
- `config_reload_total{result="parse_error"}` keeps incrementing

### 7.4 Policy authoring reference

See `docs/architecture/policy-cookbook.md` (full CEL context variables, recipe templates).

## 8. Budget and cost management

### 8.1 Budget model

```
estimated_cents = input_tokens × price_in + max_output_tokens × price_out
```

| Phase | Behavior |
|---|---|
| P0 | Reserve / Commit / Release interfaces land; record outstanding without blocking; soft 80% writes `audit_event` |
| P1 | Over hard cap → `require_approval`; soft 80% fires a webhook |

### 8.2 Configure a team budget

```yaml
# configs/identity/teams.yaml
teams:
  - team_id: acme-eng
    budget:
      monthly_cap_cents: 50000       # $500/month hard cap
      soft_limit_pct: 80            # warning at 80%
      user_daily_cap_cents: 2000    # $20/user/day
```

### 8.3 Pricing table

Edit `configs/pricing.yaml`:

```yaml
pricing:
  - model: "claude-sonnet-4-6"
    input_cents_per_mtok: 0.3       # $3/M input tokens
    output_cents_per_mtok: 1.5      # $15/M output tokens
    cache_read_cents_per_mtok: 0.03
    cache_write_cents_per_mtok: 0.375
  - model: "claude-opus-4-7"
    input_cents_per_mtok: 1.5
    output_cents_per_mtok: 7.5
```

### 8.4 Cost queries

```bash
# By team
curl -sk https://localhost:8443/api/v1/cost/breakdown?team_id=acme-eng&month=2026-05 \
  -H "Authorization: Bearer <admin-api-key>"

# By user
curl -sk https://localhost:8443/api/v1/cost/breakdown?user_id=alice&month=2026-05

# By trace_id
curl -sk https://localhost:8443/api/v1/cost/breakdown?trace_id=01hxxxx
```

The dashboard provides visual charts at `/dashboard/cost`.

## 9. Audit and compliance

### 9.1 Audit events

Every request produces an audit event written to `audit_event`:

```sql
-- Recent block events
SELECT trace_id, user_id, team_id, decision->>'primary_action' AS action,
       decision->'reasons' AS reasons, created_at
FROM audit_event
WHERE decision->>'primary_action' = 'block'
ORDER BY created_at DESC LIMIT 50;

-- Requests for a specific user
SELECT trace_id, repo_id, decision->>'model_pool' AS pool, cost_cents, created_at
FROM audit_event
WHERE user_id = 'alice' AND created_at > NOW() - INTERVAL '7 days'
ORDER BY created_at DESC;
```

### 9.2 Raw prompt access (P2+)

| Role | Permissions | Audit |
|---|---|---|
| `developer` | Own raw (non-high sensitivity) | Automatic |
| `team_admin` | Team raw (redacted); high sensitivity requires break-glass | Automatic |
| `platform_admin` | Full visibility | Every read writes `raw_access_audit` |

In the P0 / P1 phase the default is `metadata_only` (only a metadata row, no raw body persisted), so no raw is queryable.

### 9.3 GC worker

A background worker cleans up expired data hourly:

- Scans `raw_record.expires_at < now()`
- Deletes S3 objects (P2+)
- Soft-deletes metadata rows
- Writes `gc_audit` records

## 10. Incident response

### 10.1 GW unreachable

**Detection:**
- `GET /healthz` no response or timeout
- LP reports `gateway unreachable`
- Monitoring alert fires

**Diagnosis:**

```bash
# 1. Confirm the process is alive
docker ps | grep aicg-gw    # Docker Compose
systemctl status aicg-gw    # systemd

# 2. Check the logs
docker-compose logs --tail=100 gw
journalctl -u aicg-gw --since "10 min ago"

# 3. Check Postgres connectivity (GW exits if Postgres is unavailable)
docker-compose logs gw | grep -i "postgres\|database"
```

**Recovery:**

```bash
# Docker Compose
docker-compose restart gw

# systemd
systemctl restart aicg-gw
```

### 10.2 Postgres unavailable

GW behavior when Postgres is unavailable:
- Startup: retry for up to 30s, exit on timeout
- Runtime: requests fail (Postgres connection dropped); GW returns 502 to the LP

**Recovery:**

```bash
# Restart Postgres
docker-compose restart postgres

# GW will auto-reconnect
# If GW already exited, restart it
docker-compose restart gw
```

### 10.3 Provider upstream failure

GW switches automatically along the pool's configured fallback chain:

- Single endpoint fails → breaker marks it, route to another member in the pool
- Whole pool fails → fall back to `fallback_pool` (e.g., standard down → fall back to strong)
- Failure before the first token is flushed → LP retries internally once
- Failure after the first token is flushed → LP injects an `aicg.error` event and the agent retries

**Inspect breaker state:**

```bash
curl -sk https://localhost:8443/metrics | grep circuit_breaker_state
# circuit_breaker_state{provider="anthropic",model="claude-sonnet-4-6"} 1  # 1=open
```

**Manually reset breaker:**

```bash
curl -sk -X POST https://localhost:8443/_aicg/admin/circuit-breaker/reset \
  -H "Authorization: Bearer <admin-api-key>"
```

### 10.4 Policy false-block

**Symptom:** legitimate request returns 451, with `policy_reason` referring to a rule that should not have matched.

**Temporary workaround:**

```bash
# Option 1: edit the policy file and add a whitelist for the user/repo (recommended)
# Option 2: temporarily elevate the user role to platform_admin (emergency)
```

**Durable fix:** tighten the CEL condition, narrow the match scope, follow the policy change process (see §7).

### 10.5 Cost runaway

**Detection:** dashboard or `aicg stats` shows a cost spike.

**Response:**

```bash
# 1. Identify the source of the spike
SELECT user_id, team_id, repo_id, SUM(cost_cents) AS total_cents, COUNT(*) AS requests
FROM cost_event
WHERE created_at > NOW() - INTERVAL '1 hour'
GROUP BY user_id, team_id, repo_id
ORDER BY total_cents DESC LIMIT 10;

# 2. Temporary limit: add require_approval in policy
# 3. Or tighten the team budget cap
```

### 10.6 Alert escalation path

| Severity | Trigger | Response |
|---|---|---|
| P1 - Critical | GW down, company-wide outage | Immediate response, recover within 15 min |
| P2 - High | Provider fully tripped, mass policy false-block | Respond within 30 min |
| P3 - Medium | Config reload failure, secret resolution failure | Handle within 1 hour |
| P4 - Low | Budget warning, single-user anomaly | Next business day |

## 11. Backup and restore

### 11.1 Data to back up

| Data | Location | Backup method |
|---|---|---|
| Config files | `configs/` | Git repository (source of truth) |
| Postgres data | `agentgate` DB | Scheduled `pg_dump` |
| Secret files | `configs/secrets/` | Enterprise Vault / encrypted backup |

### 11.2 Database backup

```bash
# Full backup
pg_dump -h <host> -U aicg agentgate > agentgate-$(date +%Y%m%d).sql

# Schema only
pg_dump -h <host> -U aicg --schema-only agentgate > agentgate-schema.sql

# Automated backup (crontab)
0 3 * * * pg_dump -h <host> -U aicg agentgate | gzip > /backup/agentgate-$(date +\%Y\%m\%d).sql.gz
```

### 11.3 Restore

```bash
# 1. Restore the database
psql -h <host> -U aicg agentgate < agentgate-20260510.sql

# 2. Verify configuration matches the database
aicg configctl verify
```

## 12. Operations command cheatsheet

```bash
# === Service management ===
docker-compose up -d                           # Start all services
docker-compose restart gw                      # Restart GW
docker-compose restart postgres                # Restart Postgres
docker-compose logs -f gw                      # Follow GW logs
docker-compose logs --tail=200 gw              # Last 200 lines of GW logs

# === Health and status ===
curl -sk https://localhost:8443/healthz        # Basic health check
curl -sk https://localhost:8443/healthz?full=true  # Full health check
curl -sk https://localhost:8443/metrics        # Prometheus metrics
curl -sk https://localhost:8443/_aicg/version  # GW version

# === Configuration management ===
aicg policyctl validate                        # Validate policies
aicg policyctl dry-run --window 100            # Policy dry-run
aicg configctl verify                          # Config-consistency check
aicg storagectl validate                       # Storage-dependency check (P2+)

# === Routing debug ===
aicg routingctl replay <trace_id>              # Replay a routing decision

# === Database queries ===
psql -h <host> -U aicg -d agentgate
```

## 13. Upgrades

### 13.1 Upgrading the GW

```bash
# Docker Compose
git pull
make build
docker-compose up -d --no-deps --build gw

# Verify
curl -sk https://localhost:8443/_aicg/version
curl -sk https://localhost:8443/healthz
```

### 13.2 Upgrading the LP

Users upgrade themselves (no auto-update):

```bash
brew upgrade aicg          # macOS
npm update -g @agentgate/aicg  # Linux
```

On start, the LP checks whether its own version is below the minimum required by the GW (negotiated via the `X-AICG-LP-Version` header).

### 13.3 Rollback

```bash
# GW
git checkout <last-good-tag>
make build
docker-compose up -d --no-deps --build gw
```

## 14. Reference document index

| Document | Contents |
|---|---|
| `docs/architecture/SYSTEM-DESIGN.md` | Authoritative system-design document |
| `docs/architecture/policy-cookbook.md` | Policy YAML recipe set |
| `docs/architecture/threat-model.md` | STRIDE threat model |
| `docs/architecture/CHANGELOG.md` | Architecture revision log |
| `docs/dogfood-runbook.md` | Zero-to-first-request deployment |
| `docs/operations/config-reload.md` | Hot-reload + secret rotation procedure |
| `docs/operations/user-guide.md` | User guide |
