# AgentGate Dogfood Team Day-One Onboarding Guide

> Version: v0.1 | Date: 2026-05-13
> Audience: platform_admin (D-1 prep), dogfood team members (D-Day onboarding), AI Platform technical review (Week-1 retro)
> Positioning: timeline + delta guidance; details live in cross-linked existing documents

---

## 1. Audiences and roles

| Role | Responsibility | Reference section |
|---|---|---|
| platform_admin | D-1 prep of GW, team config, credential distribution | §3 |
| Dogfood team member | D-Day LP install, login, first request | §4 |
| AI Platform technical reviewer | Week-1 retro, §20.6 exit-baseline acceptance | §5 |

---

## 2. Prerequisites

- [ ] GW deployed and `/healthz` returns `db: ok` (see `dogfood-runbook.md` §Step 1–3)
- [ ] Dogfood team registered in `configs/identity/teams.yaml` (currently `team_id: dogfood`)
- [ ] CA certificate (`certs/ca.crt`) generated and ready to distribute to each member
- [ ] Each member has received the GW URL (e.g. `https://gw.example.com:8443`)
- [ ] HANDOFF-022 (`--from 7d`) merged → `aicg stats --from 7d` is available
- [ ] HANDOFF-023 (admin invite end-to-end) merged → admin issues tokens with `aicg admin invite`, members log in with `aicg login --invite` (§3.2 / §4.2)

> This document uses the placeholder `<dogfood-team-id>` for the team identifier; substitute the real one in your environment (e.g. `dogfood`).

---

## 3. D-1: Admin prep

### 3.1 Cross-check team configuration

Confirm the target team and budget cap exist in `configs/identity/teams.yaml`:

```bash
grep -A3 "team_id: <dogfood-team-id>" configs/identity/teams.yaml
# budget_monthly_cap_cents should be > 0 (e.g. 500000 = $5,000)
```

Confirm member placeholders exist in `configs/identity/users.yaml`:

```bash
grep "<dogfood-team-id>" configs/identity/users.yaml
```

> To add or modify teams or users, follow `operator-guide.md` §6.2–6.3. This document only reads to verify; it does not edit YAML.

### 3.2 Prepare member credentials

> **Current state:** HANDOFF-023 is merged. This branch's GW exposes `/api/v1/lp/exchange-invite` and `/api/v1/admin/invites` (the latter requires the `platform_admin` role); the LP CLI provides `aicg admin invite` and `aicg login --invite <token>`. Member credentials should go through the invite-token flow as the primary path; direct INSERT into `api_keys` is only the break-glass fallback when the invite service is unavailable.

**Primary path: admin generates one-shot invite tokens**

1. The admin first logs in with the setup token to obtain `platform_admin` credentials (only required for the very first admin bootstrap):
   ```bash
   docker-compose logs gw | grep "AGENTGATE BOOTSTRAP" -A 5
   aicg login --setup <setup-token> --gateway <gw-url> --ca certs/ca.crt
   ```
   > The current CLI flag parser only accepts space-separated values (see `internal/lp/cli/commands.go:33-50`); do not write `--setup=<token>` / `--gateway=<url>`.
2. Generate an invite token for each dogfood member:
   ```bash
   aicg admin invite --role developer --team <dogfood-team-id> --user dogfood-dev-1
   ```
   The token shown in the output is displayed once and single-use; deliver it through an enterprise-secure channel (1Password / Vault / encrypted mail) to the relevant member.
3. The member uses the token to log in on D-Day (see §4.2):
   ```bash
   aicg login --invite <invite-token> --gateway <gw-url> --ca /path/to/ca.crt
   ```
   On success, GW atomically consumes the invite and issues a long-lived API key for that specific member, avoiding the trap of attributing every request to `platform_admin`.

**Break-glass fallback: direct DB insert when the invite API / CLI is unavailable**

> **Critical:** the `api_keys.key_hash` column stores **bcrypt** (cost=10) hashes — not sha256/sha512 digests. GW authentication uses `bcrypt.CompareHashAndPassword(key_hash, cleartext)` (see `internal/gw/auth/pg_keystore.go:79,103`), and the setup-token exchange uses `bcrypt.GenerateFromPassword(cleartext, bcrypt.DefaultCost)` to write rows (see `internal/gw/server/handlers.go:219`). A direct INSERT of a sha256/sha512 digest will never authenticate and always returns 401. The steps below mandate bcrypt.

Only when `aicg admin invite` or `/api/v1/admin/invites` is unavailable and access must be restored immediately can the platform_admin execute this path with a change record. Do not hand the `platform_admin` credentials from setup-token login to a member directly — every request would otherwise be attributed to the admin and pollute cost attribution.

1. Manually generate a random 64-char hex key for the target member, compute the **bcrypt (cost=10)** hash, and INSERT directly into `api_keys` (schema: `internal/gw/db/migrations/0002_api_keys.up.sql`). Pick one bcrypt method:

   **A. `htpasswd` (apache2-utils; preinstalled or one-liner on most Linux/macOS workstations):**
   ```bash
   PLAIN_KEY=$(openssl rand -hex 32)
   BCRYPT_HASH=$(htpasswd -nbBC 10 "" "$PLAIN_KEY" | tr -d ':\n')
   psql -h <pg-host> -U aicg -d agentgate -c \
     "INSERT INTO api_keys (key_hash, user_id, team_id, role)
      VALUES ('$BCRYPT_HASH', 'dogfood-dev-1', '<dogfood-team-id>', 'developer');"
   echo "Send to user: $PLAIN_KEY"
   ```
   > `htpasswd -nbBC 10` outputs something like `:$2y$10$...`; stripping the leading `:` and newline with `tr -d ':\n'` yields a bcrypt string ready to use as `key_hash`. Go's `golang.org/x/crypto/bcrypt` accepts `$2a$` / `$2b$` / `$2y$` prefixes, which is isomorphic with the `$2a$` hashes produced by the setup-token path.

   **B. Python `bcrypt` (`pip install bcrypt`):**
   ```bash
   PLAIN_KEY=$(openssl rand -hex 32)
   BCRYPT_HASH=$(python3 -c "import bcrypt,sys; print(bcrypt.hashpw(sys.argv[1].encode(), bcrypt.gensalt(10)).decode())" "$PLAIN_KEY")
   psql -h <pg-host> -U aicg -d agentgate -c \
     "INSERT INTO api_keys (key_hash, user_id, team_id, role)
      VALUES ('$BCRYPT_HASH', 'dogfood-dev-1', '<dogfood-team-id>', 'developer');"
   echo "Send to user: $PLAIN_KEY"
   ```
2. Deliver the `PLAIN_KEY` (shown once, **never persisted**) + GW URL + CA cert path through a secure enterprise channel, and have the user follow §4.2 fallback to write local credentials by hand.

> **Security note:** the break-glass path bypasses the invite/audit chain; the admin must record each direct-DB-insert action (and the corresponding member) in the ticketing system. Once the incident is resolved, rotate those keys (`UPDATE api_keys SET deleted_at = now() WHERE ...` + re-issue via the invite flow).
>
> **Credential distribution:** the channel is chosen by the admin per enterprise security policy. **Do not** transmit `PLAIN_KEY` over plaintext Slack DMs, email bodies, or enterprise IM history.

### 3.3 "Quick start" snippet for the team

Package the following for team members:

```text
=== AgentGate Dogfood Onboarding Info ===
GW URL:    https://gw.example.com:8443
CA cert:   ca.crt (attached)
Your user_id: dogfood-dev-1
Login:     see docs/operations/dogfood-onboarding.md §4
Issues:    run `aicg doctor` first, then contact @platform-admin
```

---

## 4. D-Day: developer first onboarding

Run in order; each step references the more detailed user-guide section.

### 4.1 Install verification

```bash
aicg version
# Expected: agentgate v0.1.0 (abc1234)  (actual semver / git sha depend on the build artifact, see `internal/shared/version/version.go`)
```

### 4.2 Log in

**Primary path: invite token login:**

```bash
aicg login --invite <invite-token> \
           --gateway https://gw.example.com:8443 \
           --ca /path/to/ca.crt
```

> ⚠️ Same as §3.2: the current LP CLI flag parser only accepts space-separated values (`internal/lp/cli/commands.go:33-50`); the `--invite=<token>` form exits with "unknown flag" or `--invite <token> is required`. Use the space form above.

**Break-glass fallback (when the invite service is down, using the API key the admin minted):**

Write credentials by hand:

```bash
mkdir -p ~/.aicg
chmod 700 ~/.aicg
cat > ~/.aicg/credentials <<EOF
gateway_url: https://gw.example.com:8443
user_id: dogfood-dev-1
api_key: <64-char-hex-from-admin>
EOF
chmod 600 ~/.aicg/credentials
```

> **Must-read for internal-CA environments:** `aicg start` reads the CA bundle from the `ca_path` field of `~/.aicg/config.yaml` (`localconfig.Config.CAPath`, see `internal/lp/cli/commands.go:370` `loadCAPath()`). Writing `credentials` alone without `config.yaml` causes the LP to fail TLS handshake against an internally-signed GW. After copying the admin-distributed `ca.crt` locally, **also** write `config.yaml`:

```bash
cp /path/to/ca.crt ~/.aicg/ca.crt
chmod 600 ~/.aicg/ca.crt
cat > ~/.aicg/config.yaml <<EOF
port: 7777
gateway_url: https://gw.example.com:8443
ca_path: /Users/<you>/.aicg/ca.crt   # must be an absolute path
EOF
chmod 600 ~/.aicg/config.yaml
```

> If the GW certificate is signed by a public CA already trusted by the system trust store, the `ca_path` line can be omitted. Full `aicg login` parameter docs: see `user-guide.md` §3.2.

### 4.3 Set environment variables

> ⚠️ On this branch, `aicg env` only emits `export ANTHROPIC_BASE_URL=http://127.0.0.1:<port>` (**without** the `/anthropic` suffix; see `internal/lp/cli/commands.go:229`). However, §4.5 requires Claude Code to hit the LP at `http://127.0.0.1:7777/anthropic`; appending `aicg env`'s output directly to the rc file makes Claude Code return 404. Until the CLI is fixed, write the env vars explicitly:

```bash
# Explicit LP base URL (with /anthropic suffix):
cat >> ~/.zshrc <<'EOF'
export ANTHROPIC_BASE_URL=http://127.0.0.1:7777/anthropic
export ANTHROPIC_API_KEY=lp-noop
EOF
source ~/.zshrc

# Or for the current shell only (no rc file):
export ANTHROPIC_BASE_URL=http://127.0.0.1:7777/anthropic
export ANTHROPIC_API_KEY=lp-noop
```

> `user-guide.md` §3.3 lists the full environment-variable set (including `OPENAI_BASE_URL`, etc.); this guide covers only the minimal set needed for first Claude Code onboarding. Fixing `aicg env` to include the `/anthropic` suffix is a CLI change and lies outside this docs-only document.

### 4.4 Start the LP

```bash
aicg start --port 7777 &
# LP daemon listening on http://127.0.0.1:7777
```

### 4.5 Verify the first request

**curl:**

```bash
curl -s http://127.0.0.1:7777/anthropic/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: lp-noop" \
  -d '{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":"hello"}],"stream":true}'
```

**Claude Code:**

```bash
# Confirm the env var
echo $ANTHROPIC_BASE_URL
# http://127.0.0.1:7777/anthropic

claude
# Have a normal conversation; traffic automatically goes LP -> GW
```

> The path must end with `/anthropic`, with no trailing slash. See `user-guide.md` §5.1.

### 4.6 Inspect local traces

```bash
aicg status
# Should show the 5 most recent traces (with trace_id, model, provider, cost)
```

### 4.7 Self-check

```bash
aicg doctor
# Expected: [ OK ] credentials, [ OK ] config, All checks passed.
```

> Troubleshooting: see `user-guide.md` §10.

---

## 5. Week-1 retro: §20.6 exit-baseline acceptance

After 1 week of dogfooding, prove each exit baseline with the following commands.

### 5.1 Baseline 1: one-week onboarding proof

Prove the team has actual requests landing in `cost_event` over the past 7 days:

```bash
psql -h <pg-host> -U aicg -d agentgate -c \
  "SELECT count(*) FROM cost_event WHERE event_at > now() - interval '7 days' AND team_id = '<dogfood-team-id>';"
# Expected: count > 0 (the exact value depends on the team's usage frequency)
```

### 5.2 Baseline 2: p95 < 150ms

**Current path (SQL over `cost_event.latency_ms`):**

```bash
psql -h <pg-host> -U aicg -d agentgate -c \
  "SELECT
     percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms) AS p95_ms,
     COUNT(*) AS sample_size
   FROM cost_event
   WHERE event_at > now() - interval '7 days'
     AND team_id = '<dogfood-team-id>'
     AND latency_ms IS NOT NULL;"
# Expected: p95_ms < 150 and sample_size sufficient (>= hundreds) for statistical significance
```

> Column definition: `internal/gw/db/migrations/0001_init.up.sql` (`cost_event.latency_ms INTEGER`).

**Prometheus path (pending):**

```bash
curl -sk https://<gw-url>/metrics | grep "gw_request_duration_seconds_bucket"
```

> The current GW `/metrics` only registers `config_reload_total`, `secret_reload_total`, and the default Go-runtime metrics from promhttp (see `internal/gw/metrics/metrics.go`); **it does not register** `gw_request_duration_seconds_bucket`. The Prometheus histogram path belongs to follow-up observability work under R-22; until that metric lands, this baseline relies solely on the SQL path.

### 5.3 Baseline 3: availability 99% / 1 week

**Current path (SQL over `cost_event.success`):**

```bash
psql -h <pg-host> -U aicg -d agentgate -c \
  "SELECT
     COUNT(*) FILTER (WHERE success = true) * 100.0 / NULLIF(COUNT(*), 0) AS availability_pct,
     COUNT(*) AS total_requests
   FROM cost_event
   WHERE event_at > now() - interval '7 days'
     AND team_id = '<dogfood-team-id>';"
# Expected: availability_pct >= 99.0; total_requests = 0 returns NULL, indicating no baseline to prove (team has not actually onboarded or day-1 with no traffic)
```

> `NULLIF(COUNT(*), 0)` prevents a PostgreSQL `division by zero` on day-one zero traffic; when both numerator and denominator hit NULL, the team has not met the one-week onboarding baseline either, aligned with §5.1 Baseline 1.

> `cost_event` carries both the latency and success dimensions; `audit_event` has no `success` column, and its timestamp column is `event_at` (not `created_at`), so it is unsuitable for this baseline. Schema: `internal/gw/db/migrations/0001_init.up.sql`.

**Prometheus path (pending):**

```bash
curl -sk https://<gw-url>/metrics | grep "gw_requests_total"
```

> Same status as §5.2: `gw_requests_total` is not registered yet; the Prometheus count path depends on the upcoming observability handoff.

### 5.4 Baseline 4: weekly report

```bash
# Primary path (HANDOFF-022 merged)
aicg stats --by team --from 7d

# Fallback (if the stats endpoint misbehaves)
aicg stats --this-month --by team
# Then fill in precise 7-day data via psql:
psql -h <pg-host> -U aicg -d agentgate -c \
  "SELECT user_id, SUM(cost_cents) AS total_cents, COUNT(*) AS requests
   FROM cost_event
   WHERE event_at > now() - interval '7 days' AND team_id = '<dogfood-team-id>'
   GROUP BY user_id ORDER BY total_cents DESC;"
```

### 5.5 Baseline 5: replay drill

Around Day-3, pick any trace and run a replay drill to prove routing is explainable:

```bash
# 1. Take a trace_id from aicg status
aicg status

# 2. Replay
aicg routingctl replay <trace_id>
# Expected output: decision chain of attempt / pool / endpoint_id / model
```

> Full routing-debug documentation: `operator-guide.md` §12.

---

## 6. Failure fallback

> This section lists only actions executable on the current branch. Today's LP `localconfig.Config` has only `port` / `gateway_url` / `log_level` / `ca_path` (see `internal/lp/localconfig/config.go:24-29`) — no `fallback.*`; GW has registered the invite create / exchange routes but still lacks an admin API-key rotate endpoint. Recovery paths that depend on unimplemented capabilities are uniformly marked `pending`.

| Scenario | Symptom | Recovery |
|---|---|---|
| GW unreachable | LP forward returns 502 / `gateway unreachable` (see `internal/lp/server/anthropic.go:73,151`) | LP defaults to fail-closed; the admin investigates the GW process, `/healthz`, and Postgres connectivity. **Note:** the current `aicg doctor` only validates the local `credentials` / `config.yaml` / version — it **does not** probe the GW (see `internal/lp/cli/commands.go:202-222`); do not rely on `aicg doctor` to judge GW health. |
| Auto-fallback to direct provider | — | **pending**. `config.yaml` has no `fallback.on_gw_unreachable` / `direct_endpoint` (the behavior `user-guide.md` §11 describes is the R-22 target state); today the LP fails closed when GW is unreachable, with no direct-connect fallback. |
| Invite issued but unused | Token expired or lost | Admin re-runs `aicg admin invite --role developer --team <team-id> --user <user-id>` and securely distributes the new token to that member; old tokens, once expired or consumed, cannot be reused. |
| API key leak | Anomalous requests from a specific user_id | **pending**: this branch has no admin API-key rotate endpoint. The admin soft-deletes the existing key with psql, then re-issues via the §3.2 primary invite path: <br>`UPDATE api_keys SET deleted_at = now() WHERE user_id = '<user>' AND deleted_at IS NULL;`<br>(`api_keys.deleted_at` schema: `internal/gw/db/migrations/0002_api_keys.up.sql:11,14`; the partial-index predicate `WHERE deleted_at IS NULL` removes the row from the active-key set after soft delete). User side runs `aicg login --invite <token>` again. |
| Team budget over cap | `aicg stats` shows usage approaching the cap | Contact platform_admin to bump `budget_monthly_cap_cents` in `configs/identity/teams.yaml`; reload flow: `operator-guide.md` §4. |

---

## 7. Related documents

| Document | Contents | When to consult |
|---|---|---|
| `docs/dogfood-runbook.md` | GW from-zero deploy + first forwarded request | Admin first deployment |
| `docs/operations/user-guide.md` | Full developer daily-CLI reference | When a specific command issue arises on D-Day |
| `docs/operations/operator-guide.md` | Operations, configuration, monitoring, incident response | Admin D-1 prep and long-term operations |
| `docs/operations/phase-0-demo-plan.md` | Phase 0 review demo narrative | When preparing the technical-review demo |
| `scripts/dogfood-smoke.sh` | Integration smoke test (CI use) | CI validation or automated acceptance |
| `docs/architecture/SYSTEM-DESIGN.md` §18 | Phase definitions and exit baselines | Understanding where §20.6 comes from |
| `docs/architecture/SYSTEM-DESIGN.md` §20 | Implementation Readiness Checklist (R-1 ~ R-22) | Completeness acceptance |
