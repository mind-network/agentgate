# AgentGate Threat Model (STRIDE)

> Version: **v0.2.2** (2026-05-04)
> Companion docs: `SYSTEM-DESIGN.md` (architecture), `policy-cookbook.md` (policy), `CHANGELOG.md`.
> Scope: **MVP** (Phase 0–1). New components introduced from Phase 2+ (raw store / KMS / shadow_eval / SSO / SCIM / SaaS multi-tenant) require their own incremental modeling.
> Method: trust boundaries → DFD → cross-boundary STRIDE → mitigations → residual risk.
> Review cadence: every minor release (roughly every 2 months) / any new trust crossing / after any P0 security incident.

## v0.2 revision summary

- **A1 raw asset reclassification**: with the default switched to `metadata_only`, A1 (raw prompt/response) does not actually exist in P0/P1; KMS-related risk is downgraded, and the relevant STRIDE entries are annotated "applies from P2".
- **TB-9 dashboard auth contraction**: the email+password risk is removed; identity is unified to a single API key + RBAC, with the session backed by an API-key cookie.
- **RR-9 residual-risk update**: the MVP is no longer "password + bcrypt"; the residual entry is replaced by "stolen API key + short window".

---

## 1. Assets and classification

| ID | Asset | Confidentiality | Integrity | Availability | Notes |
|---|---|---|---|---|---|
| A1 | Raw prompt / response (contains source snippets, possibly PII) | High | Medium | Medium | **[v0.2] MVP P0/P1 default is `metadata_only`, no raw persisted**; raw is opt-in per repo from P2+, with KMS envelope encryption |
| A2 | Provider API keys (Anthropic / OpenAI / Azure / Bedrock / OpenRouter) | **Critical** | High | High | Leak = company billing disaster, brand disaster |
| A3 | User API keys (LP↔GW credential) | High | Medium | Medium | Can be used to impersonate the user and submit policy-visible requests |
| A4 | Repo binding token (ed25519 signed) | Medium | High | Medium | Forging it bypasses repo-level policy |
| A5 | KMS root keys / KEK | **Critical** | **Critical** | High | Compromise = all raw decryptable |
| A6 | Audit log + hash chain | Medium | **Critical** | High | Compliance foundation; tampering is a hard red line |
| A7 | Cost / routing event time series | Medium | High | Medium | Basis for financial reconciliation |
| A8 | Policy YAML / pools / budgets | Medium | **Critical** | High | Determines all authorization and routing |
| A9 | Pricing table | Low | High | Medium | If wrong, cost will be distorted but nothing leaks |
| A10 | Webhook outbound payload | Medium | Medium | Low | Carries trace metadata; must not include raw prompt |
| A11 | Dashboard session cookie / API token | Medium | Medium | Medium | Over-privileged access to raw / cost |

---

## 2. Trust boundary diagram

```
                              ┌────── Zone I: Internet (untrusted) ─────────┐
                              │                                              │
        Zone D                │                                              │
   Developer Workstation      │   Zone V: Enterprise VPC (trusted network)   │
   ╔════════════════════╗     │  ╔══════════════════════════════════════╗   │
   ║                    ║     │  ║                                      ║   │
   ║  ┌─────────────┐   ║     │  ║  ┌─────────────────────────────┐    ║   │
   ║  │ coding agent│   ║     │  ║  │      Zone G: Gateway        │    ║   │
   ║  │ (Claude Code│   ║     │  ║  │      runtime (trusted)      │    ║   │
   ║  │  / Cursor)  │   ║     │  ║  │                             │    ║   │
   ║  └──────┬──────┘   ║     │  ║  │  edge / auth / policy /     │    ║   │
   ║         │ TB-1     ║     │  ║  │  routing / provider /       │    ║   │
   ║  ┌──────▼──────┐   ║     │  ║  │  egress pipeline            │    ║   │
   ║  │  AgentGate  │◄──╫─────┼──╫──┤                             │    ║   │
   ║  │  Local Proxy│TB-3│    │  ║  └──┬─────┬─────┬─────┬────────┘    ║   │
   ║  │  (LP)       │   ║     │  ║     │TB-5 │TB-6 │TB-7 │TB-8         ║   │
   ║  └──────┬──────┘   ║     │  ║     │     │     │     │             ║   │
   ║         │ TB-2     ║     │  ║  ┌──▼──┐┌─▼──┐┌─▼──┐┌─▼──────┐     ║   │
   ║  ┌──────▼──────┐   ║     │  ║  │ PG +││ S3 ││KMS ││Webhook │     ║   │
   ║  │ ~/.aicg/    │   ║     │  ║  │ TS  ││MinIO││    ││ out    │─────╫──┐│
   ║  │ .git/aicg…  │   ║     │  ║  └─────┘└────┘└────┘└────────┘     ║  ││
   ║  └─────────────┘   ║     │  ║      Zone S: storage substrate     ║  ││
   ╚════════════════════╝     │  ╚════════════════════════════════════╝  ││
                              │                  │ TB-7                    ││
                              │                  │ provider call           ││
                              │                  ▼                          ││
                              │  ┌────────────────────────────┐             ││
                              │  │ Zone P: Upstream providers │             ││
                              │  │ Anthropic / OpenAI /       │             ││
                              │  │ Azure / Bedrock / Ollama   │             ││
                              │  └────────────────────────────┘             ││
                              │                                              ││
                              └──────────────────────────────────────────────┘│
                                                                              │
                              ┌─── Zone A: Admin operator (privileged) ──────┐│
                              │                                               ││
                              │  ╔═══════════════════════╗   TB-10            ││
                              │  ║ git / CI                                   ││
                              │  ║ (config repo)         ║──────► configs/    ││
                              │  ╚═══════════╤═══════════╝                    ││
                              │              │ TB-9 (dashboard auth)          ││
                              │              ▼                                ││
                              │   Dashboard ──────► Gateway API               ││
                              │                                               ││
                              └───────────────────────────────────────────────┘│
                                                                               │
                              External webhook receivers (Slack, SIEM, ...) ◄─┘
                                                            TB-11
```

**Enumerated trust boundaries**

| TB | Crossing point | Direction | Controls |
|---|---|---|---|
| TB-1 | coding agent → LP | loopback | Listen on 127.0.0.1 only; drop the agent's `Authorization` header |
| TB-2 | LP ↔ local files | same process | `~/.aicg/credentials` mode 0600; `.git/aicg-binding` ed25519-signed |
| TB-3 | LP → GW | D→V, HTTPS | TLS (optional mTLS) + user API key + version negotiation |
| TB-4 | Internet → GW edge | I→V | IP allowlist (default to enterprise VPN / office egress only) + TLS 1.3 + global rate-limit + WAF (optional) |
| TB-5 | GW ↔ Postgres/TimescaleDB | within V | TLS + RBAC user with least privilege |
| TB-6 | GW ↔ S3 / MinIO | within V | IAM role + bucket policy + envelope encryption |
| TB-7 | GW ↔ KMS / upstream provider | V→I→P | TLS pin + provider key resolved from a secret reference |
| TB-8 | GW → Webhook receiver | V→I | HMAC signature + URL allowlist |
| TB-9 | Admin browser → Dashboard | V/VPN | API key + RBAC + access audit |
| TB-10 | git / CI → Config FS | A→V | PR review + signed tag + admin SA token |
| TB-11 | Webhook payload → external | G→I/external | No raw prompt; HMAC |

---

## 3. Roles and capabilities

| ID | Role | Trust assumption | Key capabilities |
|---|---|---|---|
| AC1 | Honest developer | Semi-trusted (their machine has root) | Send requests, view own traces |
| AC2 | Malicious insider developer | Untrusted | Attempts to exfiltrate sensitive data via LLM traffic |
| AC3 | Compromised developer workstation | Untrusted | Process injection / LP binary swap / steal credentials |
| AC4 | External attacker (Internet) | Fully untrusted | Probe GW / phish credentials / inject |
| AC5 | Misbehaving upstream provider | Semi-trusted | May log our requests; cannot proactively exfiltrate further |
| AC6 | Malicious platform_admin | High-privilege insider | Modify policy / view any raw / change user→team mapping |
| AC7 | Compromised CI / git | High-privilege insider | Push malicious policy / change pricing / change user mapping |
| AC8 | Curious team_admin | Medium-privilege | Tries to view traces / raw outside their team |
| AC9 | Compromised KMS credentials | Critical | Decrypts all raw |

---

## 4. Data-flow diagram (DFD, key paths)

```
[agent] --(TB-1 prompt)--> [LP tagger/prescan]
   LP --(TB-3 prompt+envelope)--> [GW edge -> ingress]
   GW.ingress --(verify auth/binding/scan)--> [policy engine]
   policy --(decision)--> [routing engine]
   routing --(TB-7 wire)--> [provider]
   provider --(SSE)--> GW.egress.tap
   GW.egress --(TB-5 cost/audit)--> Postgres/TS
   GW.egress --(TB-6 raw enc)--> S3
   GW.egress --(TB-8 alert)--> webhook out
   admin browser --(TB-9 API call)--> dashboard API --> PG/TS/S3 read
   git/CI --(TB-10 push YAML)--> Config FS --(inotify reload)--> policy engine
```

> Convention: each TB is one boundary crossing; §5 below expands STRIDE for each crossing.

---

## 5. STRIDE cross-boundary table

> Acronyms: S=Spoofing, T=Tampering, R=Repudiation, I=Information disclosure, D=DoS, E=Elevation of privilege.
> Severity: P0 blocks release / P1 must-fix before release / P2 rolling fix / P3 accepted.

### TB-1 agent ↔ LP (loopback)

| Type | Threat | Severity | Mitigation | Residual |
|---|---|---|---|---|
| S | An arbitrary local process impersonates the agent and calls LP | P2 | LP listens on 127.0.0.1 only; user-level access is bounded by the machine's trust domain | Accept: local = the user themselves |
| T | A mid-stream process tampers with the prompt | P2 | Same as above; recommend `aicg start --bind 127.0.0.1` to disable network listening | Accept |
| I | A third party sniffs loopback | P3 | Same-UID-only access; macOS / Linux isolate by default | Accept |
| D | A local process floods LP | P2 | LP has built-in per-process rate-limit (rlimit fd) | Accept |
| E | LP vulnerability triggered remotely | P1 | LP does not bind to external network; refuses non-loopback listen on start; fuzz the LP HTTP parser | Residual: LP's own vulnerabilities |

### TB-2 LP ↔ local files (`~/.aicg/`, `.git/aicg-binding`)

| Type | Threat | Severity | Mitigation | Residual |
|---|---|---|---|---|
| S | An attacker writes forged credentials so LP calls a fake GW | P1 | `gateway_url` is fixed in credentials + LP validates cert pin on start (configurable) | A compromised machine can still walk away with real credentials |
| T | binding token swapped for one belonging to another repo | P1 | Binding token contains `(repo_id, machine_id, exp)`, ed25519-signed; GW validates `machine_id` is associated with the user API key | Cross-machine attack requires the API key as well |
| I | Same-UID processes can read the credential file | P1 | Mode 0600; macOS pushes Keychain (Phase 1) / Windows DPAPI; `aicg doctor` checks permissions | Same-UID processes can still read it |
| R | LP tampers with local metadata then reports to GW | P2 | metadata is a hint; the server-side reclassifier is authoritative; audit records the LP version | Only affects cost attribution |
| E | LP install path swapped | P1 | brew/npm/scoop channels + Mach-O / ELF signature check at start (Phase 1+); MVP only documents the risk | MVP residual |

### TB-3 LP → GW (HTTPS)

| Type | Threat | Severity | Mitigation | Residual |
|---|---|---|---|---|
| S | Attacker uses a stolen user API key to impersonate the user | P0 | High-entropy API key (256-bit) + server-side hash store + per-key rate limit + anomalous-IP alerts; admin can revoke instantly | Short window of abuse until revoke |
| S | MITM redirects LP traffic to a fake GW | P1 | Strong HTTPS (GW's cert chain can be anchored to the enterprise CA); LP can optionally pin; mTLS for high-security scenarios | If mTLS not enabled and enterprise CA is compromised → broken |
| T | Attacker modifies the envelope between LP and GW | P2 | TLS integrity; the server reclassifier redoes the security decision; envelope is hint only | Accept |
| R | User denies sending the request | P1 | API key uniquely maps to user_id; audit append-only + hash chain; requests carry `X-AICG-Trace-Id` + `Session-Id` | Accept (admin could abuse, see TB-9) |
| I | Packet capture reveals the prompt | P1 | TLS 1.3; plaintext HTTP is rejected by GW | Accept |
| D | A single user spams requests and exhausts GW | P1 | per-API-key token bucket + 429; per-IP edge rate limit; circuit breaker protects downstream | Residual: distributed key abuse |
| E | API key privileges abused to escalate | P0 | API key only carries the developer role; privilege bumps go through admin invite + RBAC | Accept |

### TB-4 Internet → GW edge

> Assumption: GW is typically **not** exposed directly to the public Internet; the standard deployment is a VPC + enterprise VPN / Zero Trust gateway. We still model it as "assumed exposed" so that customized deployment or operator misconfiguration is not completely defenseless.

| Type | Threat | Severity | Mitigation | Residual |
|---|---|---|---|---|
| S | External attacker brute-forces / dictionary-attacks user API keys | P0 | 256-bit high-entropy keys + server-side hash store + failure-count lockout + per-IP edge rate limit; admin gets brute-force alerts and can block IPs | Residual: distributed low-rate brute-forcing |
| S | DNS / TLS MITM redirects attackers to a fake GW | P1 | TLS 1.3 + HSTS + cert transparency (Phase 1 optional cert pin) | Accept |
| T | Edge proxy / WAF tampers with requests | P2 | Edge components listed + config in git; GW does not rely on the WAF for security decisions | Accept |
| R | Attacker replays captured requests to fabricate history | P1 | Requests must carry `X-AICG-Trace-Id` (GW-minted; a replay becomes a new trace); audit writes `client_ip` | Accept |
| I | Probing GW for unintended endpoints (e.g. `/_debug`, `/api/v1/admin/*`) | P0 | Edge router has an explicit path allowlist; admin endpoints require `Authorization` + role; `/_debug` does not exist in MVP | Residual: a future debug endpoint forgets to be authorized |
| I | Error messages leak the internal stack / paths / DB schema | P1 | Global panic recover + standardized error response (`{code, message, trace_id}`, no stack); debug mode only enabled locally | Accept |
| D | L7 DDoS / slow connections / Slowloris | P1 | Global concurrency cap at edge + read/write/idle timeouts (10/30/60s) + per-IP token bucket; front it with ALB/Nginx; large volumes go through cloud WAF | Residual: very large DDoS still requires a managed DDoS service |
| D | Oversized body / oversized header attack | P1 | `MaxRequestBodySize` default 8MB; `MaxHeaderBytes` default 64KB; **[v0.2.2] LP→GW uses a body wrapper**, separately capped via `MaxEnvelopeBytes` (envelope JSON ≤ 64KB) + `MaxWireBodyBytes` (wire.body ≤ 6MB) | Accept |
| E | Unauthenticated endpoint exploited for privilege elevation | P0 | Deny by default; endpoints explicitly tagged `public/private`; CI lint forces every handler registration to declare its role requirement | Residual: handler forgets to register |

### TB-5 GW ↔ Postgres / TimescaleDB

| Type | Threat | Severity | Mitigation | Residual |
|---|---|---|---|---|
| S | A fake GW instance connects to the DB | P1 | DB user restricted by source IP / mTLS; connection string fetched via a secret reference | Same-VPC attacker remains reachable |
| T | Direct UPDATE against the audit table | **P0** | DB user has least privilege: the app role can only INSERT/SELECT on audit; delete/update requires a break-glass admin role and is recorded in PG audit; from P3 a hash chain provides cryptographic tamper evidence | **[v0.2.3]** P0/P1 residual: **no cryptographic tamper evidence**; relies on DB privilege separation + periodic backup reconciliation + PG-level operation auditing as three soft controls |
| T | Hash chain bypassed (row deleted + chain recomputed) | P0 | **[v0.2.2 phased]** P0/P1: **hash chain not enabled**, residual risk = only DB privilege separation + periodic backup comparison, with no cryptographic tamper evidence; from P3 hash chain + daily-root dual-write to S3; P4 adds WORM | P0/P1 residual: audit records can be tampered with by an admin with DB access |
| R | Operator denies modifying the DB | P1 | DB-level audit log; platform_admin operations flow into `audit_event` | DB-level root direct connection bypasses (see P0 T) |
| I | DB backup leaks | P1 | Backup is encrypted (pg_basebackup + age/gpg); KMS still keeps raw isolated in S3 | Leaks metadata (not raw content) |
| D | OOM / connection-pool exhaustion | P1 | pgbouncer / connection-pool caps; slow-query timeouts | Accept |
| E | SQL injection | P0 | All queries are parameterized via pgx; CI lint forbids string-concatenated SQL; fuzz testing | Residual: future regression |

### TB-6 GW ↔ S3 / MinIO (raw storage)

| Type | Threat | Severity | Mitigation | Residual |
|---|---|---|---|---|
| S | A fake GW writes forged raw objects | P1 | IAM role scoped to the GW SA; bucket policy denies other principals | Accept |
| T | Direct overwrite of existing raw object | P0 | Bucket has versioning enabled + object lock (WORM in Phase 3); MVP at minimum has versioning + IAM that denies PutObject on an existing key | MVP residual: object lock not yet enabled |
| I | Obtain S3 credentials and read all raw | P0 | Envelope encryption (DEK + KMS-wrapped); S3 sees ciphertext; KMS calls audited separately | Both KMS and S3 must be compromised to decrypt → see A9 |
| I | KMS credentials leaked | P0 | KMS role limited to GW SA + multi-factor break-glass; KMS requests must carry `EncryptionContext={tenant,repo_id}`; mismatch with the trace refuses decryption | Residual: KMS provider itself is breached |
| D | Upload-rate ceiling exhausted | P2 | Raw writes are best-effort + async queue; S3 failures land in DLQ | Residual: prolonged S3 outage → DLQ buildup |
| R | Operator silently decrypts raw without trace | P0 | KMS writes an audit on every Decrypt; GW maintains a `raw_access_audit` table for break-glass | KMS audit can be disabled / altered → customer-side responsibility |

### TB-7 GW ↔ Upstream provider

| Type | Threat | Severity | Mitigation | Residual |
|---|---|---|---|---|
| S | Upstream certificate is compromised | P1 | Use provider SDK default CA + cert pin (high-security scenarios) | Accept |
| T | Upstream MITM modifies the response | P2 | TLS; post-hoc output scan; usage fields are cross-verified | Accept |
| I | Provider logs our prompt | **Not controllable** | Vet SOC2 / DPA when picking providers; high-sensitivity traffic must use private_strong (private endpoint, never leaves the enterprise) | Accept (compliance via provider choice) |
| I | Provider key leaks to the outside | P0 | **[v0.2.3 phased]** Secret resolver schemes: P0 `env://` `file://` (plaintext on the host); P1 `vault://` (if the enterprise already has Vault); P2 `aws-secretsmanager://` / `gcp-secret-manager://`; P3 KMS-managed double-wrap; rotate regularly; GW validates the key is still active at startup; cost-anomaly alerts; `secret_loaded` audit | **P0 residual**: with `env://` / `file://`, a compromised host means the key leaks; secret resolver and raw-store KMS are independent subsystems (see SYSTEM-DESIGN §10.x); do not assume P0 has KMS |
| D | Provider rate-limit cascading | P1 | Per-provider concurrent semaphore; circuit breaker; fallback chain | Residual: entire fallback chain down → 5xx |
| E | Upstream prompt-injection makes GW post-processing execute commands | P0 | GW does not act on response content (no exec, no shell, no dynamic load); webhook payloads are escaped; output scan is pattern-matching only | Accept |

### TB-8 GW → Webhook egress

| Type | Threat | Severity | Mitigation | Residual |
|---|---|---|---|---|
| S | Attacker changes the webhook URL to their own | P1 | Webhook config is platform_admin only; writes are audited; URL allowlist + DNS-rebinding defense | Residual: malicious admin |
| T | Modify payload to poison the downstream SIEM | P1 | HMAC signature + timestamp; downstream verifies | Residual: downstream does not verify |
| I | Payload includes sensitive data | P0 | Webhook does not include raw prompt; only trace_id, rule_id, metadata summary; documented schema | Residual: admin misconfigures and routes raw (disabled by default) |
| D | Slow downstream causes queue buildup in GW | P2 | Outbound retry backoff + cap; DLQ; total concurrency cap | Accept |

### TB-9 Admin / Dashboard ↔ GW API [v0.2]

> v0.2: single API key + RBAC, no email+password login. Bootstrap uses a one-shot setup_token; subsequent admin / user accounts go through invite.

| Type | Threat | Severity | Mitigation | Residual |
|---|---|---|---|---|
| S | Steal an admin API key | P0 | 256-bit high-entropy keys + server-side hash store + per-key rate limit + anomalous-IP alerts; admin self-revoke + platform_admin emergency revoke-all; Phase 3 adds OIDC + MFA | MVP residual: a stolen key can be abused until revoked |
| S | One-shot `setup_token` intercepted | P1 | Written to stderr + a `0600` file; **single-use** and immediately invalidated; startup log redacts it | Residual: machine compromised during first install |
| S | CSRF | P1 | API-key cookie with SameSite=Strict + HttpOnly; mutating operations require a custom `X-AICG-CSRF` header | Accept |
| T | Tamper with the raw view response | P0 | TLS; backend decrypts per RBAC (raw is only decryptable from P2+); no client-side decryption allowed | Accept |
| R | Admin views raw then denies | P0 | `raw_access_audit` is forced; break-glass flow requires a `purpose` field; two-person review (Phase 2) | N/A in P0/P1 (raw defaults to metadata_only) |
| I | XSS lets an attacker read raw | P0 | Dashboard uses strict React escaping; strict CSP; banned dangerous-injection APIs from rendering server-side content (enforced by lint) | Residual: third-party supply chain |
| I | IDOR — view another user's traces cross-user | P0 | Every API endpoint forces `WHERE tenant_id=? AND (user_id=? OR role >= ?)`; CI runs IDOR fixtures | Residual: new endpoints skip the lint |
| D | Report queries overload the DB | P2 | Dashboard queries hit continuous aggregates; query timeout 30s | Accept |
| E | A developer role calls an admin API | P0 | Middleware enforces RBAC; endpoint allow/deny lists are unit-tested; handler registration explicitly declares the required role | Accept |

### TB-10 git / CI → Config FS (policies / pools / pricing / users / teams)

| Type | Threat | Severity | Mitigation | Residual |
|---|---|---|---|---|
| S | Attacker pushes a fake PR to alter policy | P1 | Branch protection + two-person review + signed commits | Accept |
| T | Tamper with pricing to make cost look low | P1 | Any change to pricing.yaml requires finance + platform_admin dual codeowner approval; a reconcile job periodically diffs against the provider's public list price | Residual: two-person collusion |
| T | Add a malicious rule that diverts all traffic to an attacker-controlled endpoint | P0 | **[v0.2.2]** `pools.yaml::provider_endpoints` registry is the single endpoint source; pool members only reference `endpoint_id`; GW validates that every `endpoint_id` is in the registry on startup + reload; any URL not in the registry appearing in `routing_event.member_selected` triggers an alert; registry changes require dual codeowner approval (CODEOWNERS lists `configs/pools.yaml`) | Residual attack surface: **a single PR that simultaneously modifies the registry to add a malicious endpoint_id or alters an existing endpoint URL** (dual review can still be bypassed by collusion within the same PR) |
| T | Modify the user→team mapping to misattribute cost | P1 | users.yaml diff triggers a reconcile-job alert; trace attribution changes generate a `team_change` audit entry | Accept |
| R | Operator reloads in private | P1 | The reload API requires the platform_admin role; reload is audited | Accept |
| I | YAML contains a plaintext provider key | P0 | CI lint forbids plaintext-key patterns (regex + entropy); only `vault://` `env://` `file://` references are allowed; pre-commit hooks block it | Residual: lint misses the pattern |

### TB-11 Webhook receiver (external)

> This crossing is primarily an **outbound** threat — the concern is whether outbound leaks secrets; inbound webhooks from external sources are not accepted (MVP).
> Merged into TB-8.

---

## 6. Top-N threats (by severity + likelihood)

| Rank | Threat | Class | Asset | Severity | Likelihood | Primary mitigation | Residual |
|---|---|---|---|---|---|---|---|
| 1 | Provider key leakage | I | A2 | P0 | Medium | **[v0.2.3 phased]** P0: `env://` `file://` (plaintext on host, 0600 + EnvironmentFile); P1: `vault://`; P2: cloud SM; P3: KMS-managed double-wrap; regular rotation + cost-anomaly alerts | P0 = if the host is compromised, the key leaks; billing monitoring is the last line of defense |
| 2 | KMS credential theft | I/E | A5 | P0 | Low | KMS role isolation + EncryptionContext strict check + multi-factor break-glass | Depends on the customer's KMS ecosystem |
| 3 | Audit tampering (P0/P1 has no hash chain) | T/R | A6 | P0 | Low | **[v0.2.3 phased]** P0/P1: minimal DB roles + backup reconciliation + PG audit; P3: enable hash chain + daily-root dual-write to S3; P4: add WORM | P0/P1 residual = no cryptographic tamper evidence |
| 4 | Malicious platform_admin diverts traffic via policy | T/E | A8 | P0 | Low | Endpoint allowlist + dual review + full audit | Two-person collusion has no fix |
| 5 | Compromised developer workstation steals user API key | I/S | A3 | P1 | Medium | File mode 0600 + Keychain (Phase 1) + revoke API | MVP residual |
| 6 | IDOR across users to read raw | I/E | A1 | P0 | Medium | RBAC middleware + IDOR fixture + access audit | New endpoints may slip past the lint |
| 7 | Repo binding token forgery | S/T | A4 | P1 | Low | ed25519 signature + machine_id binding + expiry | Cross-machine attack also needs the API key |
| 8 | Webhook URL switched to an attacker URL | S/I | A10 | P1 | Low | URL allowlist + change auditing + DNS-rebinding defense | Accept |
| 9 | Provider logs our prompt | I | A1 | P1 | Medium | Provider selection on SOC2 + force private_strong for high-sensitivity traffic | Not controllable (compliance layer) |
| 10 | XSS in dashboard reads raw | I | A1 | P0 | Low | Strict CSP + React escaping + ban dangerous-injection APIs | Supply chain |
| 11 | Compromised CI pushes malicious pricing | T | A9 | P1 | Low | Codeowner dual approve + reconcile job | Accept |
| 12 | Malicious developer exfiltrates IP via LLM traffic | I | A1 | P1 | Medium | **[v0.2.2 phased]** P1: gitleaks single-engine scan + restricted_repos block + audit metadata trail (trace_id / repo / file_paths / token count); P3 adds a second scan engine + post-hoc alerts; **full raw-traceability requires P2+ per-repo opt-in `mode: full`** (the default `metadata_only` does not persist raw content) | Slow-channel (small amounts per request); P0/P1 has no raw-reverse-lookup capability |
| 13 | LP binary swapped to steal credentials | E/I | A3 | P1 | Medium | brew/npm/scoop channels + Phase 1 signature verification | MVP residual |
| 14 | Secret in streaming response not recalled | I | A1 | P1 | Low | Post-hoc scan + alert (no recall is explicitly a known limitation); rule version + periodic replay | Accept |
| 15 | DB backup leakage | I | A6/A7 | P1 | Low | Backup encryption + offline KMS that is not backed up | Raw lives in S3; backups only contain metadata |

---

## 7. Mitigation → design mapping

> Each mitigation maps to a concrete section in SYSTEM-DESIGN so nothing is implicit.

| Mitigation | Design touchpoint |
|---|---|
| Envelope encryption (DEK + KMS) | §11.2 |
| `EncryptionContext` strict check | §11.2 (KMS interface `EncContext` field) |
| Audit append-only + hash chain | §12.1 + §16.7 |
| Server-side reclassifier as authoritative for security decisions | §3.2 / §4 step 11 / cookbook §13 anti-patterns |
| Three-layer RBAC + access_audit | §11.5 / §13.4 |
| Dual-engine scan (gitleaks + detect-secrets) | §3.2 / §17 |
| Repo binding ed25519 | §16.1 / §3.2 |
| Endpoint registry | `configs/pools.yaml::provider_endpoints` registry (with `trust_tier` / `data_residency` / `supports_*`); pool members only reference `endpoint_id`; GW validates ∈ registry on startup + reload; CODEOWNERS dual approve; `routing_event.member_selected` monitors for unregistered URLs |
| Webhook HMAC + URL allowlist | §3.2 webhook + §13.7 |
| pgx parameterization + CI lint | §14 / §15 |
| Dashboard CSP / SameSite | §3.2 dashboard_ui |
| LP loopback-only listen | §5.1 / §3.3 dual_router |
| `aicg doctor` self-check (credential permissions, env conflicts, version skew) | §5.2 / §17 |
| Hash chain daily-root dual-write to S3 | §12.1 |
| KMS interface plug-in (vault / aws / gcp / local) | §11.2 / §16.6 |
| Budget hard cap → require_approval | §9.5 / cookbook §7.2 |

---

## 8. Residual risk (explicitly accepted)

> Pre-release, the platform_admin and the security owner co-sign acceptance.

| ID | Residual risk | Reason for acceptance | Trigger for re-evaluation |
|---|---|---|---|
| RR-1 | Two-person admin collusion to alter policy / pricing | No system can prevent collusion; relies on organizational process | If a single-person admin requirement arises |
| RR-2 | MVP has no LP binary signature verification | brew/npm channels already provide channel signing; Phase 1 adds it | Entry into Phase 1 |
| RR-3 | KMS provider itself is breached | Depends on customer KMS choice; external and uncontrollable | Provider incident announcement |
| RR-4 | Upstream provider logs the prompt | Compliance layer handles via provider selection + private_strong; not a technical fix | New compliance requirement |
| RR-5 | Same-UID processes on a compromised machine steal credentials | OS isolation layer; Keychain / DPAPI deferred to Phase 1 | Entry into Phase 1 |
| RR-6 | Secret in streaming response cannot be recalled post-hoc | Wire-level restriction; alert + audit are best-effort | Re-evaluate "real-time block in stream" starting Phase 1 |
| RR-7 | Slow-channel IP exfiltration (small but persistent) | A general LLM-gateway problem; MVP only alerts | Phase 2 anomaly detection |
| RR-8 | MVP single-tenant without strong multi-tenant isolation | Private deployment, single instance; multi-tenant deferred to Phase 3 | Entry into Phase 3 |
| RR-9 | MVP uses API key only (no MFA) for dashboard login | Internal tool + VPN; OIDC + MFA deferred to Phase 3 | Entry into Phase 3 or customer compliance requirement |
| RR-10 | Audit hash chain not WORM | MVP uses S3 dual-write + daily root; SOC2-grade WORM deferred to Phase 3 | Customer raises SOC2 requirement |

---

## 9. Out of scope (not modeled for MVP)

- Nation-state persistent infiltration
- Physical security (data center, office network)
- Insider collusion with more than two parties
- Provider-side supply-chain attacks
- Operator error in customer-managed KMS
- Endpoint EDR / DLP (existing enterprise stack)
- LLM "jailbreak" bypassing policy text (introduce a third-party prompt-shield in Phase 2 if needed)
- Client-side SDKs that call LLMs while bypassing the LP (this is the "not integrated with AgentGate" case; enterprise egress policy must catch it)

---

## 10. Testing and operations

### 10.1 Required security tests (CI / pre-release)

- [ ] `gosec` full-repo scan
- [ ] `go vet` + staticcheck
- [ ] Semgrep ruleset (including custom rules: forbid string-concat SQL; forbid `os/exec` appearing on the request path)
- [ ] IDOR fixture set (each dashboard endpoint has at least one cross-user IDOR case)
- [ ] Policy lint (cookbook §12.1)
- [ ] Envelope-encryption round-trip test
- [ ] Hash-chain integrity test (tamper a row → verification fails)
- [ ] LP binary refuses non-loopback listen at startup
- [ ] LP fail-closed test (GW down + default config → LP rejects requests)
- [ ] Webhook HMAC verification

### 10.2 Pre-release red-team recommendations (run at least once during Phase 0)

- Steal a user API key + replay (verify rate-limit + revoke)
- Modify a binding token across repos (verify GW check)
- Push a fake PR adding an endpoint (verify allowlist)
- Use a GW SA to directly `SELECT raw_record` (verify metadata contains no plaintext)
- Use the platform_admin role to attempt UPDATE on `audit_event` (verify DB role restrictions)
- Sniff LP↔GW traffic (verify TLS)
- Place a prompt injection in the prompt and try to coerce GW post-processing into misbehavior (verify the response is escaped)

### 10.3 Key signals to monitor (fed to dashboard / SIEM)

- Failed auth requests per minute (by IP)
- New endpoint appearing in `routing_event` (should be 0; non-zero = config drift)
- `break_glass=true` count in `raw_access_audit`
- KMS Decrypt call rate (baseline + anomalies)
- Audit-event write failure count (should be 0)
- Hash-chain `Verify` returning false on any given day (should be 0)
- Trend of `post_hoc_violation` hits
- Webhook outbound failure rate
- LP version distribution (identify long-term laggards)

---

**End of threat model v0.1.**
