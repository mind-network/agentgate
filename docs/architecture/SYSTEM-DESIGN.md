# AgentGate — Enterprise CCR-like AI Coding Gateway System Design

> Version: **v0.2.4** (MVP design, five review revisions)
> Date: 2026-05-13
> Authors: architecture / platform team
> Status: design locked, ready for implementation
> Revision log: see `CHANGELOG.md`; paragraphs tagged **[v0.2]** / **[v0.2.1]** / **[v0.2.2]** / **[v0.2.3]** / **[v0.2.4]** mark revision items.

## v0.2 revision summary

> Based on the first architecture review, all 10 items adopted. See `CHANGELOG.md` for the detailed diff.

1. **Phase 0 scope tightened** — §18 rewritten; P0 cut to ~1/3
2. **Streaming cost header removed** — §5.4: streaming uses the SSE `aicg.usage` terminating event
3. **Decision object unified as `primary_action + modifiers[] + side_effects[]`** — §7.3 §8 §16.2
4. **Budget Reserve/Commit/Release** — §9.5 §12.5 §16.8
5. **Routing selection is replayable (trace_id-derived seed)** — §9.2 §12.3
6. **IR decoupled from the Anthropic SDK** — §10 introduces our own `ir.*` types
7. **LP→GW switched to a dedicated `POST /v1/agent/forward` endpoint** — §5.4
8. **Dashboard auth collapsed to a single API-key model** — §13.1
9. **Raw defaults to `metadata_only`** — §11.4
10. **Endpoint allowlist landed in the main document** (upgraded to a `provider_endpoints` registry in v0.2.2) — §9.1

---

## 0. Product positioning and terminology

**AgentGate** is a **CCR-like coding-agent gateway** aimed at enterprise AI Platform / Engineering Productivity teams. It uniformly governs Claude Code / OpenAI-compatible coding-agent traffic inside the enterprise.

There is no goal of drop-in CCR config compatibility; the client local proxy exposes two wire-level endpoints: **Claude Code-compatible** (Anthropic Messages API) and **OpenAI-compatible** (Chat Completions).

**Key acronyms**

| Acronym | Meaning |
|---|---|
| LP | Local Proxy (client-side daemon) |
| GW | Enterprise Gateway |
| IR | Internal Representation (the gateway's internal canonical form, fixed to the Anthropic Messages API) |
| MP | Model Pool (four classes: cheap / standard / strong / private_strong) |
| PA | Provider Adapter |
| DEK / KEK | Data / Key Encryption Key |

---

## 1. Decision log

> Confirmed bidirectionally with the product owner; treated as hard constraints for implementation.

| ID | Decision |
|---|---|
| A1 | LP integration = HTTP local proxy + environment-variable hijacking |
| A2 | ~~LP↔GW protocol = pass-through upstream wire format + HTTP header / envelope JSON fields for metadata~~ **[v0.2]** LP↔GW protocol = dedicated endpoint `POST /v1/agent/forward`; body is `{envelope, wire:{protocol, body}}`; the LP still exposes Anthropic / OpenAI wire endpoints internally |
| A3 | LP behavior on GW unreachable = fail-closed (admin can configure a fallback to the cheap pool direct connection) |
| B1 | MVP identity = per-user API key + admin YAML for user→team mapping; OIDC device-code reserved for later |
| B2 | Repo identity = repo binding token (primary) + git remote URL/SHA reverse lookup (fallback) |
| B3 | Multi-tenancy = MVP is single-tenant private deployment; the code internally abstracts on `tenant_id` |
| C1 | Policy DSL = YAML + CEL, priority + deny-overrides |
| C2 | shadow_eval = MVP is a wire-level placeholder + dual emit into the table; no scorecard loop |
| C3 | require_approval = async + pending list in the dashboard |
| D1 | Output streaming scanning = post-hoc full-output scan; alert only, no blocking |
| D2 | Security scanning engine = forked gitleaks ruleset + detect-secrets |
| E1 | **[Phased from v0.2]** P0/P1: default `metadata_only` (Postgres metadata row only); P2+: S3-compatible object storage (body) + Postgres (metadata) |
| E2 | **[Phased from v0.2]** P0/P1: no KMS deployed; P2+: envelope encryption per-record DEK + KMS root key |
| E3 | **[Phased from v0.2]** P0/P1: TTL only on metadata rows; P2+: per-prefix policies driven by repo |
| F1 | Provider key = gateway-managed vault + per-team BYO co-existing |
| F2 | Cost data = provider usage fields (primary) + tokenizer estimate (fallback for local models / Ollama) |
| H1 | team_id = admin YAML maintains both user→team and repo→team mappings |
| H2 | **[Phased from v0.2.1]** P0/P1: append-only Postgres, `self_hash` optional; P3: periodic hash chain + daily root dual-write to S3 |
| Q1 | LP physical layout = single process, dual endpoints (`/anthropic/...` + `/openai/...`) |
| Q2 | Internal IR = Anthropic Messages API |
| Q3 | Anthropic-specific features when crossing providers = auto-strip + degraded_features marker; tool_use ↔ OpenAI tools / count_tokens must be real implementations |
| Q4 | **[Phased from v0.2.1]** P0: plain Postgres tables + plain indexes; P1+: TimescaleDB hypertable + continuous aggregate |
| Q5 | **[Phased from v0.2.1]** P0: Reserve/Commit/Release interfaces landed; cap is soft-warn only and non-blocking; P1+: hard-cap enforcement, triggers require_approval |
| Q6 | Client-side reporting scope = paths + language + sha256 fingerprints + diff summary stats; **does not transmit file contents** |
| Q7 | Configuration source = YAML + git ops |
| Q8 | private_strong = Azure OpenAI / Bedrock private-domain endpoints + self-hosted vLLM / TGI all belong to this tier |
| Q9 | Approval notification = generic webhook payload + dashboard pending list; not bound to Slack |
| Q10 | **[v0.2 unified]** Dashboard auth = single API key + RBAC; first start bootstraps with a one-shot setup_token; no separate password channel |
| Q11 | **[v0.2.1 phased]** Secret found in streaming response = not recalled; written to audit + alert + `post_hoc_violation` flag (**effective from P3**: the MVP does not deploy the output scanner) |
| Q12 | **[v0.2.1 phased]** P0: LP exposes the Anthropic endpoint only; P1: add the OpenAI-compatible endpoint (streaming + non-streaming) |
| Q13 | trace_id minted by the gateway; session_id minted by the client (hash of agent_pid + start_ts + repo) |
| Q14 | On fallback, each provider call writes its own `cost_event` sharing the same `trace_id` |
| Q15 | SSE mid-stream error = break the stream directly to the client; let Claude Code retry on its own |
| Q16 | Local model assumption = uniformly OpenAI-compatible (Ollama / vLLM / LM Studio / TGI / SGLang) |
| Q17 | BYO team key = YAML references a secret reference (`env://`, `file://`, `vault://`) |
| Q18 | **[v0.2 phased]** Raw prompt RBAC three layers = developer (own) / team_admin (metadata + redacted raw, break-glass) / platform_admin (full + access_audit); **takes effect from P2+**: raw defaults to `metadata_only` in P0/P1 with no persistence |
| Q19 | **[v0.2 phased]** redact = inbound text placeholder substitution `[REDACTED-{TYPE}-{HASH8}]`; the original text retained in gateway raw storage only takes effect from P2+; P1 lands the redact logic but does not retain the original (no raw store) |
| Q20 | Policy hot reload = inotify + atomic reload; new rules only apply to new requests |
| R1 | Data residency = MVP single region |
| R2 | Idempotency = internal-retry deduplication inside the gateway only; no idempotency-key exposed externally |
| R3 | Policy authoring = platform_admin only in MVP |
| R4 | Client upgrades = no auto-update; brew/npm channels + startup version check |
| R5 | Both streaming and non-streaming are supported |
| R6 | **[v0.2.4 CLI examples sync]** Onboarding = admin invite link → `aicg login --invite <token>` → long-lived API key |
| R7 | Repo binding = auto-bind on first use; binding token stored in `.git/aicg-binding`; admin can disable |
| R8 | Scan latency budget = secret pre-scan p95 must complete < 150 ms; output-scan timeouts only alert |

---

## 2. System architecture (ASCII)

```
+-----------------------------------------------------------------------------+
|                         Developer Workstation                                |
|                                                                              |
|   Claude Code            Cursor / Aider / Codex CLI / custom OAI agent       |
|   (Anthropic API)        (OpenAI Chat Completions)                           |
|        |                            |                                        |
|        | env: ANTHROPIC_BASE_URL =                                           |
|        |        http://127.0.0.1:7777/anthropic                              |
|        | env: OPENAI_BASE_URL =                                               |
|        |        http://127.0.0.1:7777/openai                                 |
|        v                            v                                        |
|   +----------------------------------------------------------------------+   |
|   |              AgentGate Local Proxy  (single binary daemon)            |   |
|   |  +----------+ +----------+ +----------+ +----------+ +-------------+ |   |
|   |  | dual-    | | session  | | metadata | | secret   | | repo        | |   |
|   |  | protocol | | / trace  | | tagger   | | pre-scan | | binding     | |   |
|   |  | router   | | id mgr   | | (heur v0)| | (gitleaks)| | manager     | |   |
|   |  +----------+ +----------+ +----------+ +----------+ +-------------+ |   |
|   |                                                                       |   |
|   |  +--------------------------+  +---------------------------------+   |   |
|   |  | local config (~/.aicg/)  |  | gateway client (mTLS, retries) |   |   |
|   |  | credentials, gw URL,     |  |                                  |   |   |
|   |  | cached policy snippets   |  |                                  |   |   |
|   |  +--------------------------+  +---------------------------------+   |   |
|   +----------------------------------------------------------------------+   |
|                                  |                                            |
|                                  | HTTPS (mTLS optional, API key required)   |
+----------------------------------|------------------------------------------+
                                   |
                                   v
+-----------------------------------------------------------------------------+
|                        AgentGate Enterprise Gateway                          |
|                                                                              |
|  +-------------------------------------------------------------------------+ |
|  |  Edge Layer  (HTTPS, IP allowlist, global rate-limit, trace_id mint)    | |
|  +-------------------------------------------------------------------------+ |
|                                  |                                           |
|                                  v                                           |
|  +-------------------------------------------------------------------------+ |
|  |  Ingress Pipeline                                                        | |
|  |    (1) auth verify -> (2) repo binding verify -> (3) server reclassify   | |
|  |    -> (4) input safety scan -> (5) policy decision -> (6) redact apply   | |
|  |    -> (7) protocol normalize (OpenAI in -> Anthropic IR)                 | |
|  +-------------------------------------------------------------------------+ |
|       |                                  |                  |                |
|       v                                  v                  v                |
|  +----------+               +-------------------+   +-----------------+      |
|  | Server   |               |  Policy Engine    |   | Budget / Quota  |      |
|  | Reclassi |<------+       |  (CEL decider +   |-->| Service         |      |
|  | -fier    |       |       |   priority/deny)  |   | (TimescaleDB +  |      |
|  +----------+       |       +-------------------+   |  Postgres)      |      |
|       |             |              ^                +-----------------+      |
|       |             |              | inotify reload                          |
|       |             |       +------+-------------+                           |
|       |             |       |  Config FS         |                           |
|       |             |       |  YAML + CEL        |                           |
|       |             |       |  (policies,        |                           |
|       |             |       |   model pools,     |                           |
|       |             |       |   budgets,         |                           |
|       |             |       |   user/team/repo)  |                           |
|       |             |       +--------------------+                           |
|       |             |                                                        |
|       |        +----+--------------+                                         |
|       |        | Routing Engine    |                                         |
|       +------->| (pool select,     |--+                                      |
|                |  fallback chain,  |  |                                      |
|                |  circuit breaker) |  |                                      |
|                +-------------------+  |                                      |
|                                       v                                      |
|       +-----------------------------------------------+                      |
|       |        Provider Adapter Layer                  |                      |
|       |  +----------+ +----------+ +-----------+      |                      |
|       |  | Anthropic| | OpenAI / | | OpenAI-   |      |                      |
|       |  |          | | Azure /  | | compatible|      |                      |
|       |  |          | | OpenRouter| | (Ollama, |      |                      |
|       |  |          | | / LiteLLM | | vLLM,    |      |                      |
|       |  |          | |           | | TGI)     |      |                      |
|       |  +----------+ +----------+ +-----------+      |                      |
|       +-----------------+-------------------------- --+                      |
|                         |                                                    |
|                         v  (streaming SSE)                                   |
|              +---------------------------+                                   |
|              | Upstream Model Providers  |                                   |
|              +---------------------------+                                   |
|                         |                                                    |
|  +----------------------|------------------------------------------------+   |
|  |  Egress Pipeline     v                                                |   |
|  |    (1) stream tap -> (2) post-hoc output scan (async) ->              |   |
|  |    (3) usage extractor -> (4) cost calc -> (5) SSE re-emit ->         |   |
|  |    (6) raw store write (envelope-encrypted)                           |   |
|  +-----------------------------------------------------------------------+   |
|         |              |              |                |                    |
|         v              v              v                v                    |
|   +----------+   +-----------+  +-----------+    +-------------+            |
|   | Postgres |   |TimescaleDB|  | S3 / MinIO|    | Webhook Out |            |
|   | audit,   |   | cost &    |  | raw body  |    | alerts,     |            |
|   | metadata,|   | routing   |  | KMS-DEK   |    | approvals,  |            |
|   | hash     |   | TS        |  | per-repo  |    | violations  |            |
|   | chain    |   |           |  | prefix    |    |             |            |
|   +----------+   +-----------+  +-----------+    +-------------+            |
|                                                                              |
|  +-----------------------------------------------------------------------+   |
|  |  Dashboard (Next.js, served by GW or separate)                        |   |
|  |  cost views | audit search | approval queue | policy RO viewer        |   |
|  |  | access audit | webhook config                                      |   |
|  +-----------------------------------------------------------------------+   |
+------------------------------------------------------------------------------+
```

---

## 3. Service decomposition and module responsibilities

### 3.1 Physical deployment units

| Unit | Form | Deployment |
|---|---|---|
| `aicg-lp` | LP daemon, single binary | Developer machine, brew / npm / scoop |
| `aicg-gw` | Gateway, single binary (with embedded dashboard static assets) | Enterprise VPC, docker-compose / helm |
| Postgres + TimescaleDB | Single instance | Same VPC |
| S3-compatible object storage | MinIO (self-hosted) or AWS S3 / GCS | Same VPC or controlled object storage |
| KMS | AWS KMS / GCP KMS / Vault Transit / local file KEK | Reuse the existing enterprise stack |

### 3.2 Logical modules (inside the gateway)

| Module | Responsibility | Key dependencies |
|---|---|---|
| `edge` | TLS termination, IP allowlist, global rate-limit, trace_id injection | chi router, `golang.org/x/time/rate` |
| `auth` | API key verification, user/team resolution, RBAC | bcrypt, Postgres |
| `repo_binding` | Verify repo binding token, git remote fallback lookup | ed25519 |
| `metadata` | Envelope parsing + server reclassifier | CEL |
| `safety` | Input scan (block/redact), output post-hoc scan (alert) | gitleaks rules + detect-secrets-py via gRPC sidecar |
| `policy` | YAML + CEL decision, priority + deny-overrides, hot reload | cel-go, fsnotify |
| `routing` | Pool selection, fallback chain, circuit breaker | sony/gobreaker |
| `provider` | Provider-adapter dispatch, transformer pipeline | Vendor SDKs |
| `transformer` | Anthropic IR ↔ OpenAI; cross-provider feature degradation | In-house |
| `budget` | Soft / hard threshold check, consumption writes | TimescaleDB |
| `cost` | Usage parsing, tokenizer estimate, pricing table | tiktoken-go, anthropic-tokenizer |
| `audit` | Append-only writes, periodic hash-chain job | Postgres |
| `raw_store` | Object-storage put/get, envelope encryption, TTL job, access audit | S3 SDK, KMS SDK |
| `webhook` | Outbound notifications (approvals / alerts / violations) | retry queue |
| `approval` | Pending list, approve/reject API | Postgres |
| `dashboard_api` | RESTful + SSE for the dashboard | chi |
| `dashboard_ui` | Next.js (built and embedded into the binary via `embed.FS`) | Next.js export |

### 3.3 LP internal modules

| Module | Responsibility |
|---|---|
| `dual_router` | Path prefixes `/anthropic/*` and `/openai/*` |
| `session` | On startup compute `session_id = sha256(agent_pid + start_ts + repo)` |
| `tagger_v0` | Heuristic inference of task_type / complexity / agentic_loop |
| `pre_scan` | gitleaks rules + custom regex |
| `repo_binder` | Auto-bind flow; binding-token caching |
| `gw_client` | mTLS, retry, timeout, version negotiation |
| `local_config` | `~/.aicg/config.yaml` + `~/.aicg/credentials` |

---

## 4. Request lifecycle [v0.2.1 full rewrite]

> Example: "Claude Code initiates a streaming `code_edit` request from a repo". Each step labels the responsible component, the phase tag, and the failure handling.
> **Phase tags**: `[P0]` = effective at MVP; `[P1]` = from Phase 1; `[P2+]` = from Phase 2.

```
Claude Code
  | (1) Anthropic Messages API request, streaming=true
  v
LP /anthropic/v1/messages                                                       [P0]
  |--- (2) Load ~/.aicg/credentials -> attach Authorization: Bearer <user-api-key>
  |--- (3) Check repo binding: read .git/aicg-binding; if missing, synchronously call GW /v1/repo/bind
  |--- (4) session/trace: inject X-AICG-Session-Id (trace_id is minted by GW)
  |--- (5) tagger_v0 emits metadata (task_type / language / file_paths / ...)
  |--- (6) pre_scan: gitleaks scan over the prompt text                          [P1]
  |       Hit -> set contains_secret_like_pattern=true into envelope.scan
  |       150ms timeout -> fail-closed, return 5xx to Claude Code
  |--- (7) Build request body: { envelope, wire:{protocol:"anthropic_messages", body:<original>} }
  |       [v0.2] No more X-AICG-Envelope header
  v
GW POST /v1/agent/forward                                                        [P0]
  |--- (8) Edge: TLS termination, per-API-key rate limit, mint trace_id (UUIDv7)
  |       Emit X-AICG-Trace-Id immediately in response headers
  v
GW Ingress
  |--- (9) auth: API key hash lookup -> user_id/team_id/role; failure -> 401     [P0]
  |--- (10) repo_binding: ed25519 verify + machine_id check; failure -> 403       [P0]
  |--- (11) Server reclassifier: recompute task_type/complexity/sensitivity      [P0]
  |          Client hints are only used as a prior weighting
  |--- (12) safety.input: gitleaks (P1) + detect-secrets sidecar (P3)            [P1]
  |          Hit + policy decides block -> 451; hit + redact -> placeholder substitution
  |--- (13) policy.Decide(...) -> Decision{                                       [P0/partial P1]
  |            primary_action,                  // P0 only `route`; P1 adds block/redact/require_approval
  |            modifiers, side_effects,
  |            model_pool, shadow_pool,
  |            redactions, reasons,
  |            require_approval_id }
  |          block -> 451 + audit; require_approval -> 202 + X-AICG-Approval-Id
  |          shadow_eval -> primary path continues (real dual emit in P2+)
  |--- (14) Protocol normalize: convert inbound OpenAI to IR; Anthropic inbound passes through  [P1, shipped with LP OpenAI endpoint]
  v
GW Budget.Reserve(estimated_cents)                                               [P0 interface / P1 enforce]
  |--- (15) estimated = input_tokens × price_in + max_output_tokens × price_out
  |          P0: only record outstanding, no blocking
  |          P1: exceeding hard cap -> return ErrHardCapExceeded -> turn into require_approval
  v
GW routing.engine
  |--- (16) seed = sha256(trace_id : pool : attempt_no); write routing_event.seed [P0]
  |          ChaCha8(seed) picks a chain by weight; fallback_pool recursion (P1 fallback/breaker)
  v
GW provider.adapter
  |--- (17) transformer.outbound: IR -> target provider wire                     [P0]
  |          Cross-provider: strip cache_control / extended thinking
  |          tool_use ↔ openai tools real conversion; record degraded_features[]
  |--- (18) Initiate upstream streaming SSE; timeout/5xx -> fallback chain (from P1) [P0/P1]
  v
Upstream Provider
  |
  v
GW egress.stream_tap                                                             [P0]
  |--- (19) Forward + accumulate into a bounded buffer (max_buffer_size)
  |          Translate back to LP per wire.protocol; inject the custom SSE aicg.usage event before stream end
  |--- (20) Post-hoc output scan -> webhook alert (non-blocking)                  [P3]
  v
GW Settle
  |--- (21) usage extractor + cost calc -> Budget.Commit(reservation_id, actual)  [P0 three tables / P1 TimescaleDB]
  |          Failure path calls Budget.Release(reservation_id)
  |          Write cost_event + routing_event
  |--- (22) raw_store.put (conditional by storage_policy)                          [P2+]
  |          metadata_only -> write raw_record metadata row only
  |          redacted_only / full -> KMS encrypt to S3
  |          P0/P1 default metadata_only -> condition branch
  |--- (23) audit.append: Postgres append-only; self_hash optional (P0/P1)         [P0]
  |          From P3 enable prev_hash chain + daily root dual-write
  v
LP <- SSE stream ->  Claude Code (passthrough upstream wire; aicg.usage consumed internally by LP)
```

**Failure / exception branches**

| Stage | Failure | Behavior |
|---|---|---|
| 6 | pre_scan timeout (P1) | LP fail-closed, 5xx + user prompt |
| 9-10 | auth/binding | 401/403, LP passes through |
| 12 | Input scan timeout (P1) | fail-closed for secret rules; fail-open for PII |
| 13 | policy block | 451 with `policy_reason` in body |
| 13 | require_approval | 202 + `X-AICG-Approval-Id`; LP prompts the user |
| 15 | Budget.Reserve fail (P1) | Per policy: turn into require_approval or return 429 |
| 18 | Upstream 5xx **before first-token flush** | LP retries internally once (same trace_id, attempt_no++); on continued failure pass 502 to the agent |
| 18 | Upstream 5xx **after first-token flush** | LP has already returned 200 OK; **status cannot be changed**; only stream-break + inject `event: aicg.error` data{partial:true}; the agent retries itself |
| 20 | Post-hoc scan hit (P3) | No recall; webhook alert + audit `post_hoc_violation=true` |
| 22 | raw_store.put failure | best-effort async DLQ; does not affect the response |
| GW unreachable | LP fail-closed (default) or admin-configured fallback to cheap pool direct connect | |

---

## 5. Client adapter interface design

### 5.1 HTTP endpoints exposed by the LP

```
http://127.0.0.1:<port>/anthropic/v1/messages
http://127.0.0.1:<port>/anthropic/v1/messages/count_tokens
http://127.0.0.1:<port>/openai/v1/chat/completions
http://127.0.0.1:<port>/openai/v1/models           (passthrough; filtered by allowlist)
http://127.0.0.1:<port>/_aicg/health
http://127.0.0.1:<port>/_aicg/version
http://127.0.0.1:<port>/_aicg/whoami
http://127.0.0.1:<port>/_aicg/sessions             (debug)
```

### 5.2 LP CLI

```
aicg login --invite <token>           # [v0.2.4] exchange an invite for a long-lived API key
aicg logout
aicg start [--port 7777] [--config <path>] [--foreground]
aicg stop
aicg status
aicg bind-repo [--path .]             # manual bind (use when auto-bind fails)
aicg config show
aicg config set gateway_url=...
aicg policy show                       # pull a summary of applicable policy from GW
aicg version
aicg doctor                            # self-check: gateway reachable, credentials, binding, env vars
```

### 5.3 Environment-variable integration

`aicg env` prints a shell snippet that the developer sources or appends to their rc file:

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:7777/anthropic"
export ANTHROPIC_API_KEY="sk-aicg-noop"   # LP swaps in the real credential
export OPENAI_BASE_URL="http://127.0.0.1:7777/openai/v1"
export OPENAI_API_KEY="sk-aicg-noop"
```

The LP drops the agent-supplied `Authorization` and attaches its own user API key to GW (GW identifies the user from this and then substitutes the appropriate provider key downstream).

### 5.4 LP→GW protocol [v0.2]

> The v0.1 design was "LP passes through upstream wire + envelope in headers". The review pointed out that a realistic envelope (64 file paths + fingerprints + diff summary + scan findings) easily exceeds the 8KB header limit, and falling back to body breaks the "transparent wire" principle — that design is half-baked by construction.
> v0.2 changes to: LP→GW uses a dedicated envelope endpoint; the LP↔agent loopback still exposes the upstream-wire `/anthropic/...` / `/openai/...` unchanged.

#### 5.4.1 LP→GW endpoint

```
POST <gateway>/v1/agent/forward
```

**Required request headers**

- `Authorization: Bearer <user-api-key>` (injected by the LP)
- `X-AICG-LP-Version: <semver>`
- `X-AICG-Session-Id: <session_id>`
- `X-AICG-Repo-Binding: <signed-binding-token>`
- `Accept: text/event-stream` (streaming) or `application/json` (non-streaming)

**Request body**

```json
{
  "envelope": { /* full AICGEnvelope object, see §6 */ },
  "wire": {
    "protocol": "anthropic_messages" | "openai_chat_completions",
    "stream":   true,
    "body":     { /* original upstream provider request body, byte-for-byte */ }
  }
}
```

After receiving: GW parses the envelope → runs the ingress pipeline → policy → routing → provider → egress → writes the response back **in the upstream wire's SSE / JSON** per `wire.protocol`; the LP forwards transparently to the agent (the agent sees only the upstream protocol; nothing else).

#### 5.4.2 Response headers (only at the very first send)

- `X-AICG-Trace-Id: <uuid>`
- `X-AICG-Decision: <primary_action>` (allow|block|route|require_approval)
- `X-AICG-Modifiers: <comma-list>` (redact|escalate_to_strong_model)
- `X-AICG-Side-Effects: <comma-list>` (shadow_eval|log_only)
- `X-AICG-Reasons: <comma-list-of-rule-ids>`
- `X-AICG-Routed-To: <provider>:<model>`
- `X-AICG-Degraded-Features: <comma-list>`
- `X-AICG-Cost-Cents: <int>` — **non-streaming responses only**; streaming uses §5.4.3
- `X-AICG-Approval-Id: <id>` — when require_approval

#### 5.4.3 Carrying cost / termination info in a stream (the `aicg.*` custom SSE events) [v0.2.2 schema formalized]

> v0.2 correction: HTTP/1.1 SSE cannot append regular headers once the body has started; HTTP/2 trailers are not read by most client SDKs. Streaming no longer promises `X-AICG-Cost-Cents`.
> v0.2.2 makes the event schema strict and clarifies LP passthrough rules and debug behavior.

**Event namespace**: all AgentGate metadata events use the `aicg.` prefix; they cannot collide with upstream wire event names (`message_start`, `content_block_delta`, etc.).

**Event 1: `aicg.usage`** (always present at stream end; not emitted for non-streaming)

```
event: aicg.usage
id:    <trace_id>
data:  <JSON object>
```

```json
{
  "schema_version": "1.0",
  "trace_id": "01hxxx...",
  "session_id": "abc...",
  "cost_cents": 42,
  "cost_source": "provider_usage",        // "provider_usage" | "estimated"
  "tokens": {
    "input": 1024,
    "output": 512,
    "cache_read": 0,
    "cache_create": 0
  },
  "decision": {
    "primary_action": "route",
    "modifiers": [],
    "side_effects": [],
    "model_pool": "standard",
    "reasons": ["P-ROUTE-002"]
  },
  "routed_to": "anthropic-prod:claude-opus-4-7",   // endpoint_id:model
  "attempt_no": 1,
  "degraded_features": [],
  "latency_ms": 8230,
  "partial": false
}
```

**Event 2: `aicg.error`** (only emitted on failure after a token has already been flushed; pre-first-token failures use HTTP error codes)

```
event: aicg.error
id:    <trace_id>
data:  <JSON object>
```

```json
{
  "schema_version": "1.0",
  "trace_id": "01hxxx...",
  "code": "upstream_5xx",                 // upstream_5xx | upstream_timeout | upstream_disconnect | gw_internal
  "provider_status": 502,                 // optional
  "message": "anthropic returned 502 mid-stream",
  "partial": true,
  "tokens_emitted_so_far": 318
}
```

**LP passthrough rules**

| Event | LP→agent passthrough | LP internal handling |
|---|---|---|
| Upstream wire events (`message_start` / `content_block_delta` / `message_stop` / ...) | ✅ verbatim | Content not parsed |
| `aicg.usage` | ❌ **not passed through** | After parsing, update the local ledger (`aicg status` / `aicg.tracesdb`) for dashboard fallback queries |
| `aicg.error` | ❌ **not passed through** | Translate to the upstream wire's "stream ended early" semantics: (a) for the Anthropic wire, inject `event: message_stop` + `data: {stop_reason: "error"}`; (b) for the OpenAI wire, inject `data: [DONE]` |

> Upstream SDKs (Anthropic SDK / OpenAI SDK) only look at upstream event names; behavior on unfamiliar `event: aicg.*` lines is SDK-implementation-specific (most ignore, a few panic). The LP must intercept them to keep SDK behavior stable.

**Debug / direct-curl mode**

The LP accepts a `--passthrough-aicg-events` flag (dev only) that disables interception so `aicg.*` events go straight to stdout:

```bash
aicg start --foreground --passthrough-aicg-events
# then:
curl -N http://127.0.0.1:7777/anthropic/v1/messages -d @req.json | grep '^event: aicg'
```

Production deployments force this flag to false (the systemd unit / process refuses to start with it set).

**Clients without SSE-event parsing** can call `GET /api/v1/cost/breakdown?trace_id=...` to fetch the equivalent of `aicg.usage`.

#### 5.4.4 Failure semantics [v0.2.1 clarified HTTP status constraints]

> Key constraint: **once the HTTP response status line has been emitted (200 OK + `Content-Type: text/event-stream`), the status cannot be changed.** LP retries are only valid before the **first content token** has been flushed to the agent.

LP's internal state machine (in the direction of the agent):

```
[no status]   --first-byte-from-GW-->  [200 OK headers flushed]  --any-token-->  [streaming]
     |                                          |                                       |
     | GW 5xx / disconnect                      | GW 5xx but no real token yet         | GW 5xx already flushed token
     v                                          v                                       v
  LP decides:                              LP can still:                          LP can only:
  (a) retry internally fallback (once)     (a) retry internally fallback (once)   - inject event: aicg.error
  (b) still failing -> 502 / 503 to agent  (b) still failing -> inject            - data: {trace_id, partial:true}
                                              event: aicg.error in the open 200    - close SSE stream
                                              stream, then close                    - cannot change status (200 already)
                                          (c) cannot change status
```

| Scenario | Behavior |
|---|---|
| Upstream 5xx, **LP has not flushed any token** | LP internal retry once; still failing → pass 502/503 to the agent; audit `partial=false` |
| Upstream 5xx, **LP has already flushed at least one token** | LP must keep the 200 OK; only inject `event: aicg.error` + `data: {trace_id, code, partial:true}` and close; the agent sees the SSE ending early and decides on its own retry |
| Non-streaming upstream 5xx | GW walks the fallback chain; final failure returns 502 + JSON body |
| GW unreachable | LP fail-closed (default) or fall back to direct cheap pool (admin configured) |

**Implementation note**: the LP-side SSE handler holds a "may retry" flag before writing the first byte; it is cleared the moment the first frame is written. All retry decisions complete before the first byte.

---

## 6. Metadata envelope schema

```yaml
# JSON Schema (informal)
AICGEnvelope:
  schema_version: "1.0"
  client:
    lp_version: string                  # e.g. "0.3.1"
    os: enum[darwin,linux,windows]
    arch: enum[amd64,arm64]
  identity:                              # client-side hints; the server recomputes / verifies
    user_id: string
    team_id: string                     # client-reported, GW corrects via admin YAML
    machine_id: string                  # stable hash of hostname+install
  agent:
    tool: enum[claude_code, cursor, aider, codex_cli, continue, custom_oai]
    tool_version: string?
    wire_protocol: enum[anthropic_messages, openai_chat_completions]
  session:
    session_id: string                  # sha256(agent_pid+start_ts+repo)
    turn_index: int                      # N-th turn within the session
    is_continuation: bool                # whether tool_result is included (agentic-loop continuation)
  repo:
    repo_id: string?                     # the id from the GW-issued repo binding (trusted)
    remote_url: string?                  # client-reported, verified by GW
    branch: string?
    head_sha: string?
    is_dirty: bool?
  context_signals:                       # heuristic tagger output
    file_paths: [string]                 # relative paths (deduped, capped at 64 entries)
    file_fingerprints:                   # no content sent
      - path: string
        size_bytes: int
        sha256: string
        language: string?
    diff_summary:                        # statistics only, no diff text
      lines_added: int
      lines_removed: int
      contains_test_failure_keyword: bool
      contains_stack_trace: bool
    primary_language: string?
  task_hints:                            # heuristic v0 client labels
    task_type: enum[planning, architecture, repo_search, file_reading,
                    simple_edit, code_edit, test_output, debug, review,
                    security_review, summary, unknown]
    complexity: enum[low, medium, high, unknown]
    data_sensitivity: enum[low, medium, high, unknown]
    agentic_loop: bool
    contains_secret_like_pattern: bool
    contains_pii_like_pattern: bool
    security_sensitive_area: bool
    routing_hints: [string]              # free-form hints, log only
    confidence:                          # client confidence per hint [0,1]
      task_type: float
      complexity: float
      data_sensitivity: float
  scan:
    pre_scan_engine: "gitleaks-fork@<rev>"
    pre_scan_findings:                   # finding type + position only; **no plaintext**
      - rule_id: string
        severity: enum[info,warn,critical]
        offset: int
        length: int
  custom_tags:                           # custom extension (capped at 16 KB)
    {string -> string|number|bool}
```

**Contract notes**

- The envelope is a **hint** in its entirety; the server reclassifier unconditionally recomputes `task_type / complexity / data_sensitivity`; client values feed the `confidence` table as a prior.
- The envelope does not carry prompt text (avoid duplication + reduce attack surface); prompts travel via the wire body.
- `repo.remote_url` and the `repo_id` inside the `repo_binding` token must be validated for consistency by GW.

---

## 7. Policy engine design

### 7.1 YAML + CEL DSL

```yaml
# policies/main.yaml
version: 1
defaults:
  on_no_match: { action: route, model_pool: standard, reasons: ["default-fallthrough"] }

rules:
  - id: P-SEC-001
    description: "Restricted repos block hard secrets pattern"
    priority: 1000          # larger = evaluated first
    when: |
      envelope.repo.repo_id in restricted_repos
      && (envelope.task_hints.contains_secret_like_pattern
          || server_class.has_secret_finding)
    action: block
    reasons: ["restricted-repo + secret-detected"]

  - id: P-SEC-002
    description: "Any provider key / private key -> redact"
    priority: 990
    when: |
      server_class.findings.exists(f, f.type in ["aws_key","gcp_key","private_key","jwt"])
    action: redact

  - id: P-ROUTE-001
    description: "summary / test_output / repo_search -> cheap"
    priority: 500
    when: |
      server_class.task_type in ["summary","test_output","repo_search"]
      && envelope.task_hints.data_sensitivity != "high"
    action: route
    model_pool: cheap

  - id: P-ROUTE-002
    description: "code_edit / simple_edit / planning -> standard"
    priority: 500
    when: |
      server_class.task_type in ["code_edit","simple_edit","planning"]
    action: route
    model_pool: standard

  - id: P-ROUTE-003
    description: "architecture / debug / security_review -> strong"
    priority: 500
    when: |
      server_class.task_type in ["architecture","debug","security_review"]
    action: route
    model_pool: strong

  - id: P-ROUTE-004
    description: "Restricted repo or high sensitivity -> private_strong"
    priority: 700
    when: |
      envelope.repo.repo_id in restricted_repos
      || server_class.data_sensitivity == "high"
    action: route_to_private_model
    model_pool: private_strong

  - id: P-BUDGET-001
    description: "Team monthly budget hit hard cap -> escalate to require_approval"
    priority: 800
    when: |
      budget.team_monthly_used_cents >= budget.team_monthly_cap_cents
    action: require_approval

  - id: P-EVAL-001
    description: "Shadow eval: 5% of code_edit also sent to strong for evaluation"
    priority: 100
    when: |
      server_class.task_type == "code_edit" && rand() < 0.05
    action: shadow_eval
    model_pool: strong

variables:
  restricted_repos: ["repo_payments_core", "repo_keys_vault"]
```

### 7.2 Decision flow [v0.2.1 aligned with the three-slot model]

```
Inputs:
  envelope        (client-reported)
  server_class    (server-side reclassifier output)
  user, team, repo (auth resolved)
  budget          (Budget.Check snapshot, with outstanding reservations)
  rand()          (built-in, deterministically derived from trace_id)

Steps:
  1. Load all rules; sort by priority descending
  2. Evaluate `when` expressions (CEL) in order; collect all matched rules
  3. Apply the three-slot merge algorithm (see §8.2):
     - PrimaryAction: single-pick, deny-overrides; block > require_approval > route* > allow
     - Modifiers:     0..N cumulative (redact, escalate_to_strong_model)
     - SideEffects:   0..N cumulative (shadow_eval, log_only)
  4. Emit Decision{
       primary_action,
       modifiers[],
       side_effects[],
       model_pool,
       shadow_pool,
       redactions[],
       reasons:[rule_ids],
       degraded_features[]   // populated during routing
     }
```

### 7.3 Decision object [v0.2]

> The review pointed out that v0.1's §7.3 had a single `Action`, §8 supported redact + route + shadow_eval stacking, and §16.2 introduced `Modifiers` — three inconsistent shapes. v0.2 collapses them into a **three-slot model**.

```go
type Decision struct {
    PrimaryAction     string          // single terminal slot: allow|block|route|route_to_private_model|require_approval
    Modifiers         []string        // stackable modifiers: redact, escalate_to_strong_model
    SideEffects       []string        // side effects: shadow_eval, log_only
    ModelPool         string          // primary routing: cheap|standard|strong|private_strong
    ShadowPool        string          // filled when SideEffects include shadow_eval
    Redactions        []RedactionSpec // filled when Modifiers include redact
    Reasons           []string        // matched rule_ids

    // [v0.2.3] Endpoint constraints — the policy does not iterate candidate members;
    //         it emits constraints; the RoutingEngine filters the registry during BuildChain
    RequiredTrustTier      string   // "" | "vendor" | "partner" | "private"  (minimum allowed tier)
    RequiredDataResidency  []string // allowed data_residency list, e.g. ["eu"], ["us","on_prem"]; empty = unrestricted
    RequiredCapabilities   []string // required supports.* keys, e.g. ["cache_control","tools"]; empty = no requirement

    DegradedFeatures  []string        // populated during routing
    RequireApprovalID string          // filled when PrimaryAction == require_approval
}
```

**Policy / Routing responsibility split** [v0.2.3]

- Policy (runs before routing) only sees envelope / server_class / user / team / repo / budget; it does not know about candidate pool members.
- The reason policy can read `endpoints["..."].*` in CEL is to **emit constraints** (e.g. derive `required_data_residency=["eu"]` based on `repo.tags`), not to pick endpoints.
- The RoutingEngine filters the `provider_endpoints` registry inside `BuildChain` by the constraints: keep endpoints with `trust_tier ≥ required` ∧ `data_residency ∈ required_residency` ∧ `supports[k] == true ∀ k ∈ required_capabilities`.
- Empty member set after filtering → routing failure: return 502 + audit `routing_no_candidate_after_constraints`.

**Slot semantics**

| Slot | Cardinality | Terminating | Examples |
|---|---|---|---|
| `PrimaryAction` | single-pick | Decides whether the request enters routing | `route`, `block`, `require_approval` |
| `Modifiers` | 0..N | Mutates request content or routing preference; does not flip allow/deny | `redact` (replace prompt text), `escalate_to_strong_model` (bump pool up) |
| `SideEffects` | 0..N | No effect on the primary path; produces additional events | `shadow_eval` (dual emit to ShadowPool), `log_only` (write audit + webhook, non-blocking) |

**Example**

```
{
  primary_action: "route_to_private_model",
  modifiers:      ["redact"],
  side_effects:   ["shadow_eval"],
  model_pool:     "private_strong",
  shadow_pool:    "strong",
  redactions:     [{type:"aws_key", offset:1024, length:40}],
  reasons:        ["P-SEC-REDACT-001", "P-SENS-UP-003", "P-EVAL-001"]
}
```

---

## 8. Policy conflict resolution [v0.2]

**Core algorithm**: `priority desc + deny-overrides`, resolved against the §7.3 three-slot model.

> v0.2 revision: terminology fully aligned with §7.3 / §16.2. **There is no generic "action" slot**; the `action:` field in the rule file is mapped to one of the three slots at parse time.

### 8.1 Rule-file field → slot mapping

In YAML you write:

```yaml
- id: P-...
  action: <verb>            # single-pick
  modifiers: [<verb>, ...]  # optional
  side_effects: [<verb>, ...] # optional
```

`<verb>` category table (**`action:` for a single rule must be from the PrimaryAction column**; `modifiers:` / `side_effects:` each from its own column):

| Verb | Slot | Meaning |
|---|---|---|
| `allow` | PrimaryAction | Equivalent to `route(default_pool)`; pass through |
| `block` | PrimaryAction | terminal-deny; 451 + audit |
| `route` | PrimaryAction | Enter routing; pool from `model_pool` |
| `route_to_private_model` | PrimaryAction | route specialization; pool forced to private_strong |
| `require_approval` | PrimaryAction | terminal-pending; write to the approval queue |
| `redact` | Modifier | Inbound text placeholder substitution; continue to route |
| `escalate_to_strong_model` | Modifier | Bump the picked pool to strong (no-op if already strong/private_strong) |
| `shadow_eval` | SideEffect | Duplicate the request to the ShadowPool referenced by `model_pool`; not returned to the client |
| `log_only` | SideEffect | No effect on the decision; only writes audit + fires webhook |

### 8.2 Merge algorithm

```
hits = filter(rules, r => CEL(r.when) == true) sorted by priority desc

PrimaryAction:
  blocks = [r for r in hits if r.action == "block"]
  if blocks: result.PrimaryAction = "block"; result.Reasons = [highest_priority_block.id]; return
  approvals = [r for r in hits if r.action == "require_approval"]
  if approvals: result.PrimaryAction = "require_approval"; result.RequireApprovalID = mint(); ...
  routes = [r for r in hits if r.action in ("route","route_to_private_model","allow")]
  if routes: take highest priority -> result.PrimaryAction & ModelPool
  else: apply defaults.on_no_match

Modifiers (non-terminating; everything accumulates):
  for r in hits:
    for m in r.modifiers: result.Modifiers.add(m)
    if r.action == "redact": result.Modifiers.add("redact")          # backward-compat shorthand
    if r.action == "escalate_to_strong_model": result.Modifiers.add(...)

SideEffects (everything accumulates):
  same as above; shadow_eval / log_only all added

result.Reasons = unique(rule_ids of all contributing hits)
```

### 8.3 Worked examples

**Example 1: block reason vetoes**

```
Matched: R1 block prio=1000 / R2 redact prio=990 / R3 route_to_private_model prio=700
Result:  PrimaryAction=block, Reasons=[R1]
```

**Example 2: modifier + side_effect stacking**

```
Matched: R2 redact prio=990 / R3 route_to_private_model prio=700 / R5 shadow_eval[strong] prio=100
Result: {
  PrimaryAction: "route_to_private_model",
  Modifiers: ["redact"],
  SideEffects: ["shadow_eval"],
  ModelPool: "private_strong",
  ShadowPool: "strong",
  Reasons: [R2, R3, R5]
}
```

**Example 3: fallthrough**

```
Matched: 0 rules
Result:  defaults.on_no_match → route(standard)
```

**Conflict worked example**

```
Matched:
  R1 block  prio=1000
  R2 redact prio=990
  R3 route_to_private_model prio=700
  R4 route[standard] prio=500
  R5 shadow_eval[strong] prio=100

Decision:
  block matched -> terminal -> action=block, reasons=[R1]
  (R2-R5 not processed further)
```

```
Matched:
  R2 redact prio=990
  R3 route_to_private_model prio=700
  R5 shadow_eval[strong] prio=100

Decision:
  redact is a modifier -> stacked
  terminal-allow picks R3 -> route_to_private_model (private_strong)
  shadow_eval is a side-effect -> stacked
  Final: action=redact+route_to_private_model+shadow_eval, pool=private_strong
```

**Exception**: no rule matches → `defaults.on_no_match`.

---

## 9. Routing engine design

### 9.1 Model pool configuration [v0.2.1 endpoint_id registry]

> Review v0.2.1 #4: v0.2's allowlist stored full URLs, but pool member fields are logical names (e.g. `endpoint: "ollama-cluster"`) — the two cannot match directly. v0.2.1 introduces a `provider_endpoints` registry as the single source of truth; pools only reference `endpoint_id`; the allowlist is the set of `endpoint_id` values.

```yaml
# pools.yaml

# (1) Endpoint registry [v0.2.2 extended attributes]
#   - The only allowed list of physical endpoints
#   - Carries compliance / trust / capability attributes so policies can reference them directly
#     (avoiding scattered `variables` lists)
provider_endpoints:
  anthropic-prod:
    provider: anthropic
    url:      "https://api.anthropic.com"
    data_residency: us
    trust_tier:     vendor                # vendor | partner | private
    supports:
      streaming: true
      tools: true
      cache_control: true
      extended_thinking: true
  openai-prod:
    provider: openai
    url:      "https://api.openai.com/v1"
    data_residency: us
    trust_tier:     vendor
    supports:
      streaming: true
      tools: true
      cache_control: false
      extended_thinking: false
  openrouter-prod:
    provider: openrouter
    url:      "https://openrouter.ai/api/v1"
    data_residency: us
    trust_tier:     vendor
    supports: { streaming: true, tools: true, cache_control: false, extended_thinking: false }
  azure-tenant-1:
    provider:   azure_openai
    url:        "https://acme-tenant.openai.azure.com"
    deployments: ["ent-gpt4o", "ent-gpt4o-eu"]
    data_residency: eu
    trust_tier:     private               # enterprise-owned tenant
    supports: { streaming: true, tools: true, cache_control: false, extended_thinking: false }
  bedrock-us-east-1:
    provider: anthropic_bedrock
    url:      "bedrock-runtime.us-east-1.amazonaws.com"
    data_residency: us
    trust_tier:     private
    supports: { streaming: true, tools: true, cache_control: true, extended_thinking: true }
  ollama-cluster:
    provider: openai_compat
    url:      "https://ollama-cluster.internal:11434/v1"
    data_residency: on_prem
    trust_tier:     private
    supports: { streaming: true, tools: false, cache_control: false, extended_thinking: false }
  vllm-prod:
    provider: openai_compat
    url:      "https://vllm-prod.internal:8000/v1"
    data_residency: on_prem
    trust_tier:     private
    supports: { streaming: true, tools: true, cache_control: false, extended_thinking: false }
  litellm-internal:
    provider: litellm
    url:      "https://litellm-proxy.internal:4000"
    data_residency: us
    trust_tier:     partner
    supports: { streaming: true, tools: true, cache_control: false, extended_thinking: false }

# (2) GW startup / reload checks:
#   - Every pool member's endpoint_id must be in provider_endpoints
#   - Registry changes require codeowner dual approval (CODEOWNERS lists configs/pools.yaml)
#   - Any URL not in the registry appearing in routing_event.member_selected fires an immediate alert
#
# (3) Policy may reference directly [v0.2.3 correction: constraint output, not iteration]:
#   - Policy runs before routing and **does not** receive candidate members.
#   - In CEL, reading endpoints[<id>] is used to generate constraint fields on Decision:
#       required_trust_tier, required_data_residency, required_capabilities
#   - The RoutingEngine filters the registry by these constraints during BuildChain to pick compliant candidates.
#   Example policy CEL:
#     when:  "eu-data-residency" in repo.tags
#     emit:  required_data_residency = ["eu", "on_prem"]
#   Future EU-only / private-only / cache_control-required rules are all expressed via required_* constraints,
#   with no scattered variables and no endpoint picking in policy.

pools:
  cheap:
    members:
      - { endpoint_id: openrouter-prod, model: "deepseek/deepseek-chat", weight: 70 }
      - { endpoint_id: ollama-cluster,  model: "qwen2.5-coder:7b",      weight: 30 }
    fallback_pool: standard
    max_attempts: 2
    timeout_ms: 60000

  standard:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-sonnet-4-6", weight: 60 }
      - { endpoint_id: openai-prod,    model: "gpt-4o",            weight: 40 }
    fallback_pool: strong
    max_attempts: 3
    timeout_ms: 90000

  strong:
    members:
      - { endpoint_id: anthropic-prod, model: "claude-opus-4-7", weight: 80 }
      - { endpoint_id: openai-prod,    model: "o3",              weight: 20 }
    fallback_pool: null
    max_attempts: 2
    timeout_ms: 180000

  private_strong:
    members:
      - { endpoint_id: azure-tenant-1,    deployment: "ent-gpt4o", model: "gpt-4o", weight: 60 }
      - { endpoint_id: bedrock-us-east-1, model: "anthropic.claude-opus-4-7",       weight: 40 }
    fallback_pool: null
    max_attempts: 1                  # private domain does not tolerate cross-domain fallback
    timeout_ms: 240000
```

> The `private` flag comes from the `provider_endpoints` registry; pool members do not redeclare it.

### 9.2 Selection algorithm [v0.2 seed / v0.2.3 with constraint filter]

> Review v0.2 #5: weighted random must be replayable (trace_id-derived seed).
> Review v0.2.3 #1: `BuildChain` consumes the constraints emitted by `Decision` to filter the registry.

```
SelectChain(pool, decision, trace_id, attempt_no):
  seed = sha256(trace_id || ":" || pool || ":" || attempt_no)[:16]   # [v0.2]
  rng  = ChaCha8(seed)                                                # [v0.2]
  1. Read pool.members; resolve each member's endpoint attributes from the registry
  2. Filter by constraints [v0.2.3]:
       member.endpoint.trust_tier >= decision.required_trust_tier
       AND (decision.required_data_residency is empty OR
            member.endpoint.data_residency in decision.required_data_residency)
       AND (for all cap in decision.required_capabilities: member.endpoint.supports[cap] == true)
  3. Filter out members whose circuit breaker is open
  4. Weighted-sample order [m1, m2, ...] using rng
  5. If fallback_pool is not null, recursively SelectChain(fallback_pool, decision, trace_id, attempt_no)
       and append to the tail (fallback must also satisfy the same constraints)
  6. Truncate to max_attempts
  7. Write [seed, applied_constraints] into routing_event columns                # [v0.2 / v0.2.3]
  8. If the post-filter set is empty -> return ErrNoCandidate; ingress turns it into 502 + audit
       "routing_no_candidate_after_constraints"

Execute(chain, request):
  for i, m in enumerate(chain):
    try:
      resp = adapter.send(m, request, timeout=pool.timeout_ms)
      if resp.ok: return resp
    except RetryableError [5xx, network, timeout]:
      breaker(m).recordFailure()
      record routing_event(attempt_no=i, error=...)
      continue
    except NonRetryable [4xx auth, 400 invalid]:
      return error
  return last_error
```

Replay tool: `aicg routingctl replay <trace_id>` rebuilds the chain from `routing_event.seed` and verifies that the decision is reproducible.

### 9.3 Circuit breaker

- `sony/gobreaker`, per `(provider, model)`
- Threshold: 10 consecutive failures, or > 50% failure rate within 1 minute → open
- half-open: allow 1 probe request after 30 seconds

### 9.4 Streaming failure semantics [v0.2.2 reference §5.4.4]

> The single authoritative definition is in §5.4.4. The routing engine's failure handling follows that state machine:
>
> - After any token has been flushed to the agent, **status cannot be changed**; only inject `event: aicg.error` and close
> - Mid-stream provider 5xx does **not** auto-fallback (stitching semantics are too brittle)
> - Internal retries are only allowed before the LP flushes the first token to the agent (once)
> - See §5.4.4 for the state machine and the implementation note on the LP "may retry" flag

### 9.5 Rate-limit / quota / budget [v0.2.1 phased]

> Review v0.2.1 #7: Reserve has different semantics in P0 vs P1; this must be made explicit.

| Dimension | Phase | Implementation |
|---|---|---|
| Per-API-key request rate | P0 | In-memory token bucket; over-limit returns 429 |
| Per-team monthly $ | **P0** | Reserve / Commit / Release interfaces land; outstanding recorded for CLI reports; **non-blocking**; soft 80% **only writes audit_event** + red flag in `aicg stats` output (**P0 does not fire a webhook** — the webhook subsystem ships in §18 P1) |
| Per-team monthly $ | **P1** | Reserve returns `ErrHardCapExceeded` when over the hard cap; ingress translates that into `require_approval`; soft 80% also fires the webhook |
| Per-user daily $ | same | same |
| Per-provider concurrent | P0 | Semaphore (prevent blowing through a provider's quota) |

**Estimation formula** (same from P0)

```
estimated_cents
  = input_tokens   x pricing.input_cents_per_mtok / 1_000_000
  + max_output_tokens x pricing.output_cents_per_mtok / 1_000_000
```

`max_output_tokens` uses the request's explicit value; if unspecified, use the pool default (a conservative upper bound).

**Settlement**

- Normal completion: routing succeeds → cost extractor computes `actual_cents` → `Commit(reservation_id, actual_cents)`; the delta `(estimated - actual)` is returned automatically
- Provider failure: `Release(reservation_id)`, full refund
- Client disconnect: `Release` is triggered in a `ctx.Done()` defer
- Process crash: a background settler scans `created_at < now - 10min && state == reserved` every minute → `Release` + alert (Reserve-leak alert)

Detailed schema: §12.5. Interface signatures: §16.8.

---

## 10. Provider adapter interface [v0.2 IR decoupled from SDK]

> Review #6: v0.1's IR directly referenced `anthropic.MessageParam` and similar SDK types, binding us to a specific SDK version and reproducing the CCR implicit-mutate pitfall. v0.2 introduces our own `ir.*` types. Anthropic SDK types only appear inside the `provider/anthropic.go` adapter.

### 10.0 IR own types (`internal/shared/ir/types.go`)

```go
package ir

// Semantically "Anthropic-Messages-API-shaped" but with independent fields - no SDK types.
type Message struct {
    Role    Role             // user|assistant|tool
    Content []ContentBlock
}

type Role string
const (
    RoleUser      Role = "user"
    RoleAssistant Role = "assistant"
    RoleTool      Role = "tool"   // tool_result uses a dedicated role in our IR
)

type ContentBlock struct {
    Type        BlockType
    Text        string             // Type == Text
    ToolUse     *ToolUse           // Type == ToolUse
    ToolResult  *ToolResult        // Type == ToolResult
    Thinking    *ThinkingBlock     // Type == Thinking
    CacheControl *CacheControlSpec // segment-level cache marker
}

type BlockType string
const (
    BlockText        BlockType = "text"
    BlockToolUse     BlockType = "tool_use"
    BlockToolResult  BlockType = "tool_result"
    BlockThinking    BlockType = "thinking"
)

type Tool struct {
    Name        string
    Description string
    InputSchema map[string]any   // JSON Schema
}

type ToolChoice struct {
    Type ToolChoiceType
    Name string                  // when Type == Specific
}

type ToolChoiceType string
const (
    ToolChoiceAuto    ToolChoiceType = "auto"
    ToolChoiceAny     ToolChoiceType = "any"
    ToolChoiceSpecific ToolChoiceType = "specific"
)

type ToolUse struct {
    ID    string
    Name  string
    Input map[string]any
}

type ToolResult struct {
    ToolUseID string
    Content   []ContentBlock     // usually Text only
    IsError   bool
}

type ThinkingBlock struct {
    Text string
}

type CacheControlSpec struct {
    Type string                 // "ephemeral"
    TTL  string                 // optional: "5m", "1h"
}

type ThinkingSpec struct {
    Enabled      bool
    BudgetTokens int
}

type StopReason string
const (
    StopEndTurn   StopReason = "end_turn"
    StopMaxTokens StopReason = "max_tokens"
    StopToolUse   StopReason = "tool_use"
    StopStopSeq   StopReason = "stop_sequence"
)
```

### 10.1 Adapter interface

```go
// internal/gw/provider/adapter.go
package provider

import "agentgate/internal/shared/ir"

type IRRequest struct {
    Model       string
    Messages    []ir.Message
    System      []ir.ContentBlock          // system may contain cache_control
    Tools       []ir.Tool
    ToolChoice  *ir.ToolChoice
    MaxTokens   int
    Temperature *float64
    Stream      bool
    Thinking    *ir.ThinkingSpec
    Metadata    map[string]string
}

type IRStreamEvent struct {
    Type IRStreamEventType    // message_start | content_block_start | content_block_delta |
                              // content_block_stop | message_delta | message_stop |
                              // aicg_usage (internally generated)
    Raw  []byte                // Serialized event (re-converted when emitted in the downstream wire)
    // Parsed strongly-typed fields (populated per Type)
    BlockIndex int
    Delta      *ir.ContentBlock
    Usage      *Usage
}

type IRResponse struct {
    ID         string
    Model      string
    Content    []ir.ContentBlock
    StopReason ir.StopReason
    Usage      Usage
}

type Usage struct {
    InputTokens         int
    OutputTokens        int
    CacheReadTokens     int
    CacheCreationTokens int
}

type Adapter interface {
    // Identifier
    Name() string                          // "anthropic", "openai", "azure_openai", "openai_compat", "anthropic_bedrock"
    SupportsStreaming() bool
    SupportsTools() bool
    SupportsCacheControl() bool
    SupportsExtendedThinking() bool

    // Send non-streaming
    Send(ctx context.Context, req IRRequest, member PoolMember) (*IRResponse, error)

    // Send streaming; events are pre-converted back to Anthropic IR events
    SendStream(ctx context.Context, req IRRequest, member PoolMember) (<-chan IRStreamEvent, <-chan error)

    // Token counting (used by /v1/messages/count_tokens)
    CountTokens(ctx context.Context, req IRRequest, member PoolMember) (int, error)
}

type PoolMember struct {
    EndpointID string                       // [v0.2.1] references the provider_endpoints registry (§9.1)
    Provider   string                       // resolved from the endpoint registry; not configured separately
    URL        string                       // same as above
    Model      string
    Deployment string                       // for azure_openai
    Private    bool                         // resolved from the registry
    KeyRef     SecretRef                    // env://, file://, vault://
    ExtraOpts  map[string]any
}
```

**Target adapter list & phase matrix [v0.2.1]**

| Adapter | P0 | P1 | P2 | P3 |
|---|:-:|:-:|:-:|:-:|
| `anthropic` (direct Anthropic API) | ✅ | | | |
| `openai_compat` (Ollama / vLLM / LM Studio / TGI / SGLang / self-hosted) | ✅ | | | |
| `openai` (direct OpenAI) | | ✅ | | |
| `openrouter` (shares the OpenAI wire) | | | ✅ | |
| `litellm` (shares the OpenAI wire) | | | ✅ | |
| `azure_openai` (Azure OpenAI Service) | | | | ✅ |
| `anthropic_bedrock` (AWS Bedrock) | | | | ✅ |

> P0 picks `anthropic` + `openai_compat`: the former covers the main Claude Code traffic, the latter covers enterprise self-hosted and local inference at no extra transformer cost. Direct OpenAI is deferred to P1 alongside the LP OpenAI-compatible endpoint. Azure / Bedrock are enterprise hardening (P3) — they need IAM / tenant integration.

### 10.x Secret resolver (independent of the raw-store KMS) [v0.2.3 clarification]

> Review v0.2.3 #4: do not conflate "provider key retrieval wrapped by KMS" with the raw store's "DEK envelope encryption" — they are **separate subsystems** with independent phasing.

| Subsystem | Purpose | P0 | P1 | P2 | P3 |
|---|---|:-:|:-:|:-:|:-:|
| **Secret resolver** | At startup, resolves `provider_endpoints[*].key_ref` and other secret references → in-memory cleartext provider API key | `env://`, `file://` ✅ | `vault://` ✅ (if the enterprise already runs Vault) | `aws-secretsmanager://`, `gcp-secret-manager://` ✅ | KMS-managed secret (double-wrap) ✅ |
| **Raw store KMS** | DEK envelope encryption to write raw prompt/response into S3 | — | — | KMS abstraction ships (aws_kms / gcp_kms / vault_transit / local_file_kek) ✅ | Multi-KMS / per-tenant / customer-managed keys ✅ |

**Interface contract**

```go
// internal/shared/secretref/resolver.go
type Resolver interface {
    // One-shot resolve at startup; schemes: env, file, vault, aws-secretsmanager, gcp-secret-manager
    Resolve(ctx context.Context, ref string) (plaintext []byte, err error)
}
```

**P0 residual risk**: with only `env://` / `file://`, provider keys are present in cleartext in the GW process memory and the systemd unit's environment; a host compromise leaks them. Mitigations:

- `file://` files at mode 0600 and readable only by the GW SA
- `env://` only via systemd `EnvironmentFile=`, not `Environment=` (avoids `ps eww` leakage)
- Audit entry `secret_loaded` (records only the ref, never the plaintext)

The threat-model lists this residual under TB-7 / Top-1 (see `threat-model.md`).

### 10.1 Transformer responsibilities

- **inbound**: OpenAI Chat Completions request → IR (only when the LP route is `/openai/...`)
- **outbound**: IR → target provider wire
  - Same family (Anthropic→Anthropic, OpenAI→OpenAI-style): passthrough
  - Cross family (Anthropic IR → OpenAI-style):
    - Reshape `messages` into OpenAI's `messages` (merge system, tool_use → `tool_calls`, tool_result → `tool` role)
    - Strip `cache_control`; `degraded_features += ["cache_control"]`
    - Strip `thinking`; `degraded_features += ["extended_thinking"]`
    - Pass `stop_sequences` through
- **inbound stream**: rewrite OpenAI SSE deltas into Anthropic IR `content_block_delta`
- **count_tokens**: when crossing providers, estimate locally using anthropic-tokenizer and append `estimated: true`

---

## 11. Raw prompt / response storage design

### 11.1 Physical model

```
S3 object key:
  raw/tenant=<tid>/repo=<repo_id>/sensitivity=<low|med|high>/dt=YYYY-MM-DD/<trace_id>.bin

Object body layout (binary):
  | magic "AICG\x01" 5B
  | header_len uint32 |
  | header_json (encrypted-DEK envelope:
       { dek_wrapped_b64, kek_id, alg: "AES-256-GCM", nonce_b64, redacted_fields:[...] }
     ) |
  | ciphertext (AES-256-GCM, AAD = trace_id) |
```

### 11.2 Encryption pipeline

```
plaintext = canonical_json({ request: <ir_request>, response: <ir_response>, sse_events: [...] })

DEK = csprng(32 bytes)
ciphertext = AES-GCM(DEK, plaintext, AAD=trace_id)
DEK_wrapped = KMS.Encrypt(KEK_id, DEK, context={tenant, repo_id})

object = magic | header_len | header_json | ciphertext
```

KMS interface abstraction:

```go
type KMS interface {
    Encrypt(ctx context.Context, kekID string, plaintext []byte, ctx EncContext) ([]byte, error)
    Decrypt(ctx context.Context, kekID string, ciphertext []byte, ctx EncContext) ([]byte, error)
    GenerateDataKey(ctx context.Context, kekID string, spec string) (plaintext, wrapped []byte, error)
}
```

Implementations: `aws_kms` / `gcp_kms` / `vault_transit` / `local_file_kek` (dev).

### 11.3 Metadata table [v0.2.1 compatible with metadata_only]

> Review v0.2.1 #2: `object_uri/object_size/kek_id` have no values in `metadata_only` mode. Make them nullable + use a `storage_policy` column + CHECK constraint for consistency.

```sql
-- raw_record (Postgres) — single table covering metadata_only / redacted_only / full
CREATE TABLE raw_record (
    trace_id        UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    team_id         TEXT NOT NULL,
    repo_id         TEXT,
    sensitivity     TEXT CHECK (sensitivity IN ('low','medium','high','unknown')),
    storage_policy  TEXT NOT NULL CHECK (storage_policy IN
                       ('metadata_only','redacted_only','full','disabled')),
    -- v0.2.1: the three columns below are filled only for redacted_only / full
    object_uri      TEXT,
    object_size     INTEGER,
    kek_id          TEXT,
    redacted_fields JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL,
    deleted_at      TIMESTAMPTZ,

    -- Consistency constraint: storage_policy and the object columns must match
    CONSTRAINT raw_record_object_consistency CHECK (
      (storage_policy IN ('redacted_only','full')
         AND object_uri IS NOT NULL AND object_size IS NOT NULL AND kek_id IS NOT NULL)
      OR
      (storage_policy = 'metadata_only'
         AND object_uri IS NULL AND object_size IS NULL AND kek_id IS NULL)
    )
);
CREATE INDEX ON raw_record (tenant_id, repo_id, created_at DESC);
CREATE INDEX ON raw_record (expires_at) WHERE deleted_at IS NULL;

-- disabled mode does not write this table at all (egress pipeline skips)
```

**Column-state matrix**

| `storage_policy` | object_uri | object_size | kek_id | redacted_fields | egress behavior |
|---|---|---|---|---|---|
| `metadata_only` (P0/P1 default) | NULL | NULL | NULL | NULL | Write metadata row only |
| `redacted_only` (P2+) | filled | filled | filled | filled | redact → DEK encrypt → S3 |
| `full` (P2+) | filled | filled | filled | NULL | DEK encrypt → S3 |
| `disabled` | — | — | — | — | Skip the table entirely |

### 11.4 Default storage policy and TTL [v0.2 default changed to metadata_only]

> Review #9: v0.1's 30-day full raw default was both a sales blocker and a compliance blocker. v0.2 defaults to `metadata_only`; `redacted_only` / `full` must be opted in per repo. MVP P0 does not deploy KMS / object storage (see §18).

- Default `default_storage_policy: metadata_only` (only writes the `raw_record` metadata row; no S3 object; no KMS call)
- Default TTL 30 days (metadata rows)
- `mode` values:

| mode | Behavior |
|---|---|
| `metadata_only` | `raw_record` row only; no S3 object |
| `redacted_only` | `safety.redact_engine` replaces placeholders pre-store; DEK encrypted to S3 |
| `full` | Full prompt + response; DEK encrypted to S3 |
| `disabled` | No write at all |

```yaml
storage:
  default_storage_policy: metadata_only         # [v0.2]
  default_retention_days: 30
  repo_overrides:
    repo_payments_core:
      mode: disabled                             # extremely sensitive: keep nothing
    repo_eval_corpus:
      mode: full                                 # explicit opt-in for evaluation
      retention_days: 90
    repo_debug_pool:
      mode: redacted_only
      retention_days: 14
```

**Minimum conditions to upgrade raw-capture capability**

- `mode != metadata_only` requires: (a) the KMS provider referenced in `pools.yaml` is configured; (b) the object-storage bucket is verified writable; otherwise the startup validation fails.
- Deployment check: `aicg storagectl validate` reports each repo's effective mode + the status of physical dependencies.

- Background job (`gc_worker`) scans `expires_at < now()` hourly; calls KMS `ScheduleKeyDeletion` if supported, deletes S3 objects, soft-deletes metadata rows, writes `gc_audit`.

### 11.5 Access control + access audit

- `developer`: can access raw under their own user_id (non-high sensitivity)
- `team_admin`: same team; high-sensitivity defaults to a redacted view, needs break-glass
- `platform_admin`: fully visible; every access writes `raw_access_audit`

```sql
CREATE TABLE raw_access_audit (
    id              BIGSERIAL PRIMARY KEY,
    accessed_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    accessor_user   TEXT NOT NULL,
    accessor_role   TEXT NOT NULL,
    target_trace_id UUID NOT NULL,
    target_repo_id  TEXT,
    purpose         TEXT,                     -- filled by the caller (debug, audit, eval, ...)
    break_glass     BOOLEAN NOT NULL DEFAULT FALSE,
    ip              INET,
    user_agent      TEXT
);
```

### 11.6 Repo-level disable / redacted_only

- `mode: disabled` → egress pipeline skips `raw_store.put`; no metadata row either
- `mode: redacted_only` → before encryption, `safety.redact_engine` replaces identified secrets / PII with placeholders
- audit still records everything (audit ≠ raw)

---

## 12. Audit / cost data model

### 12.1 Audit [v0.2.1 hash chain phased]

> Review v0.2.1 #8: Phase 0 does not deploy a hash chain, but the schema with `self_hash NOT NULL` blocked writes. Make it nullable + enable per phase.

```sql
-- Always append-only; the hash chain only turns on in P3
CREATE TABLE audit_event (
    id              BIGSERIAL PRIMARY KEY,
    event_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    trace_id        UUID NOT NULL,
    session_id      TEXT,
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    team_id         TEXT NOT NULL,
    repo_id         TEXT,
    event_type      TEXT NOT NULL,
    decision        JSONB,             -- v0.2 three-slot Decision snapshot
    rule_ids        TEXT[],
    detail          JSONB,
    request_summary JSONB,             -- envelope + classifier (no prompt text)
    routed_to       TEXT,              -- "endpoint_id:model"
    fallback_chain  JSONB,
    error_code      TEXT,
    -- chain (filled from P3; NULL allowed in P0/P1)
    prev_hash       BYTEA,             -- [v0.2.1] nullable; not written before P3
    self_hash       BYTEA              -- [v0.2.1] nullable; not written before P3
);
CREATE INDEX ON audit_event (trace_id);
CREATE INDEX ON audit_event (tenant_id, team_id, event_at DESC);
CREATE INDEX ON audit_event (tenant_id, repo_id, event_at DESC);

-- P3 migration: when enabling the chain there is no need to backfill history (P0/P1 rows keep NULL hashes; forward-only chain from P3)
-- After audit.chain.enabled=true at startup, BEFORE INSERT trigger writes self_hash on new inserts
-- The audit_chain_root table starts receiving daily roots at the same time
```

**Phase agreements**

| Phase | self_hash | prev_hash | Periodic root job |
|---|---|---|---|
| P0 / P1 / P2 | NULL | NULL | Not running |
| From P3 | sha256(canonical(row)) | previous row's self_hash | Every day at 00:05 UTC, compute Merkle root by `(tenant_id, id asc)`; write `audit_chain_root` + a signed copy to S3 at `audit-chain/<date>/<tenant>.json` |

Historical data (NULL hashes) does not participate in the chain, but pre-P3 audit can still serve as a compliance archive; for integrity tracing, combine with backup reconciliation.

### 12.2 Cost [v0.2.1 P0 plain Postgres / P1 to TimescaleDB]

> Review v0.2.1 #3: P0 does not deploy TimescaleDB; the schema must first land as plain tables, then P1 migrates to hypertables.

#### P0 schema (plain Postgres)

```sql
CREATE TABLE cost_event (
    id              BIGSERIAL PRIMARY KEY,
    event_at        TIMESTAMPTZ NOT NULL,
    trace_id        UUID NOT NULL,
    attempt_no      SMALLINT NOT NULL,         -- 1=primary, 2..=fallback
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    team_id         TEXT NOT NULL,
    repo_id         TEXT,
    task_type       TEXT,
    policy_rule_id  TEXT,
    provider        TEXT NOT NULL,
    endpoint_id     TEXT NOT NULL,             -- [v0.2.1] references provider_endpoints
    model           TEXT NOT NULL,
    pool            TEXT NOT NULL,
    is_private      BOOLEAN NOT NULL,
    input_tokens    INTEGER NOT NULL,
    output_tokens   INTEGER NOT NULL,
    cache_read_tokens     INTEGER DEFAULT 0,
    cache_create_tokens   INTEGER DEFAULT 0,
    cost_cents      INTEGER NOT NULL,
    cost_source     TEXT NOT NULL,              -- 'provider_usage' | 'estimated'
    latency_ms      INTEGER,
    success         BOOLEAN NOT NULL,
    error_class     TEXT
);
CREATE INDEX ON cost_event (event_at DESC);
CREATE INDEX ON cost_event (tenant_id, team_id, event_at DESC);
CREATE INDEX ON cost_event (tenant_id, user_id, event_at DESC);
CREATE INDEX ON cost_event (tenant_id, repo_id, event_at DESC);
CREATE INDEX ON cost_event (provider, model, event_at DESC);
```

The P0 dashboard aggregations run direct SQL (GROUP BY day); at internal-dogfood volume (< 1M rows/month) there is no bottleneck.

#### P1 migration to TimescaleDB

```sql
-- 0001_p1_timescale_migration.sql
-- 1. Enable extension
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- 2. Convert to hypertable (migrate_data does the online migration)
SELECT create_hypertable('cost_event', 'event_at',
                         chunk_time_interval => INTERVAL '7 days',
                         migrate_data => true);

-- 3. Continuous aggregate
CREATE MATERIALIZED VIEW cost_daily
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 day', event_at) AS day,
    tenant_id, team_id, user_id, repo_id, provider, model, pool, task_type,
    SUM(input_tokens)  AS input_tokens,
    SUM(output_tokens) AS output_tokens,
    SUM(cost_cents)    AS cost_cents,
    COUNT(*)           AS n_requests,
    SUM(CASE WHEN success THEN 0 ELSE 1 END) AS n_failed
FROM cost_event
GROUP BY day, tenant_id, team_id, user_id, repo_id, provider, model, pool, task_type;

SELECT add_continuous_aggregate_policy('cost_daily',
    start_offset => INTERVAL '7 days',
    end_offset   => INTERVAL '1 hour',
    schedule_interval => INTERVAL '30 minutes');
```

### 12.3 Routing event (dedicated to routing decisions and degradation)

```sql
-- P0: plain Postgres table
CREATE TABLE routing_event (
    event_at        TIMESTAMPTZ NOT NULL,
    trace_id        UUID NOT NULL,
    attempt_no      SMALLINT NOT NULL,
    tenant_id       TEXT NOT NULL,
    decision        JSONB NOT NULL,            -- {primary_action, modifiers, side_effects, ...}
    pool_selected   TEXT,
    member_selected JSONB,                     -- {endpoint_id, provider, model, private}
    fallback_chain  JSONB,
    degraded_features TEXT[],
    breaker_state   TEXT,
    seed            BYTEA,                     -- [v0.2] sha256(trace_id:pool:attempt_no)
    PRIMARY KEY (trace_id, attempt_no)
);
CREATE INDEX ON routing_event (event_at DESC);
CREATE INDEX ON routing_event (tenant_id, event_at DESC);

-- P1 migration: SELECT create_hypertable('routing_event', 'event_at',
--                       chunk_time_interval => INTERVAL '7 days', migrate_data => true);
```

### 12.5 Budget reservation [v0.2 new]

> Review #4: the hard cap cannot rely on after-the-fact `Consume`.

```sql
CREATE TABLE budget_reservation (
    id              UUID PRIMARY KEY,
    trace_id        UUID NOT NULL,
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    team_id         TEXT NOT NULL,
    estimated_cents INTEGER NOT NULL,
    actual_cents    INTEGER,                  -- nullable until commit
    state           TEXT NOT NULL,            -- 'reserved' | 'committed' | 'released' | 'expired'
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    settled_at      TIMESTAMPTZ
);
CREATE INDEX ON budget_reservation (state, created_at);
CREATE INDEX ON budget_reservation (tenant_id, team_id, created_at DESC);
```

Materialized view (includes the reserved portion) for the dashboard "reserved + settled" display:

```sql
CREATE VIEW budget_team_monthly_v AS
SELECT
    tenant_id, team_id,
    date_trunc('month', created_at) AS month,
    SUM(CASE WHEN state IN ('reserved','committed') THEN
        COALESCE(actual_cents, estimated_cents) ELSE 0 END) AS used_cents
FROM budget_reservation
GROUP BY tenant_id, team_id, month;
```

**Settler background job**: scans `state='reserved' AND created_at < now() - INTERVAL '10 minutes'` every minute → auto `Release` + write audit `reservation_expired` (also raises an alert — indicating a Reserve leak somewhere in the GW path).

### 12.4 Approval

```sql
CREATE TABLE approval_request (
    id              UUID PRIMARY KEY,
    trace_id        UUID NOT NULL,
    tenant_id       TEXT NOT NULL,
    requestor       TEXT NOT NULL,
    team_id         TEXT NOT NULL,
    repo_id         TEXT,
    decision_snapshot JSONB NOT NULL,
    state           TEXT NOT NULL,            -- pending|approved|rejected|expired
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMPTZ,
    resolver        TEXT,
    note            TEXT
);
```

---

## 13. Dashboard APIs

> All RESTful + JSON; streaming interfaces use SSE. Every response carries `X-AICG-Trace-Id`.

### 13.1 Auth & users [v0.2 single API-key model]

> Review #8: v0.1 had three concurrent shapes: (a) the client API-key system, (b) email + password login, and (c) the threat-model line "MVP password + bcrypt only". v0.2 collapses these into **single API key + RBAC**.

```
GET   /api/v1/whoami                       # echo: {user_id, role, team_id}
POST  /api/v1/admin/invites                # platform_admin mints invites (role: developer|team_admin|platform_admin)
POST  /api/v1/admin/users/{id}/role        # change role
GET   /api/v1/admin/users
GET   /api/v1/me/api-keys                  # current user's key list (returns last4 + created_at only)
POST  /api/v1/me/api-keys                  # self-mint a key (plaintext returned once)
DELETE /api/v1/me/api-keys/{id}            # revoke
POST  /api/v1/admin/users/{id}/api-keys/revoke-all   # platform_admin emergency revoke
```

**Bootstrap**: GW auto-generates a one-shot `setup_token` on first start (written to stderr and `/var/lib/agentgate/setup-token.txt`); admins use:

```
aicg login --gateway https://gw.internal --setup <token>  # [v0.2.4] CLI uses space-separated flag values
```

Exchanges for the first `platform_admin` API key; `setup_token` invalidates immediately. Subsequent admin / user provisioning all goes through the invite flow.

**Dashboard**: uses the same API key as `Authorization: Bearer`; the UI carries it via an HttpOnly + SameSite=Strict cookie; there is no separate email/password login.

**No logout endpoint**: revocation = revoke the API key. After Phase 1 introduces OIDC device-code, the session concept will be added.

### 13.2 Cost / routing views

```
GET /api/v1/cost/summary
    ?dim=team|user|repo|model|provider|task_type|policy_rule
    &group_by=day|week|month
    &from=...&to=...&team_id=...&repo_id=...
    -> [{ bucket, dim_value, cost_cents, input_tokens, output_tokens, n_requests, n_failed }]

GET /api/v1/cost/breakdown
    ?trace_id=... | ?session_id=...
    -> Single trace's fallback chain and per-attempt cost

GET /api/v1/routing/recent
    ?team_id=...&limit=...
    -> Most recent routing events with degraded_features

GET /api/v1/budget
    -> Current budget state: team monthly / user daily (with soft/hard thresholds and usage)
```

### 13.3 Audit search

```
GET /api/v1/audit/search
    ?trace_id=... | ?user_id=... | ?repo_id=... | ?event_type=... | ?from=...&to=...
    &cursor=...&limit=50
    -> { items:[audit_event], next_cursor }

GET /api/v1/audit/{trace_id}
    -> Event chain + routing + cost + raw reference for one trace (raw visibility per RBAC)

GET /api/v1/audit/chain/verify?date=YYYY-MM-DD
    -> Verify a day's hash-chain root
```

### 13.4 Raw prompt access (RBAC + access_audit gated)

```
GET /api/v1/raw/{trace_id}?purpose=debug&break_glass=false
    -> Decrypted output (auto redacted view based on role / sensitivity)
    side-effect: writes raw_access_audit
```

### 13.5 Approval

```
GET  /api/v1/approvals?state=pending
POST /api/v1/approvals/{id}/approve   { note }
POST /api/v1/approvals/{id}/reject    { note }
GET  /sse/v1/approvals/stream         # Long-poll SSE; pushes pending changes
```

### 13.6 Policy / pool / config (MVP read-only)

```
GET  /api/v1/policy/current             # Parsed YAML (platform_admin only)
GET  /api/v1/pools/current
POST /api/v1/policy/reload              # Trigger hot reload (platform_admin only)
GET  /api/v1/policy/decision/explain    # Simulate decision: pass envelope + classifier, get Decision + matched rules
```

### 13.7 Webhook configuration

```
GET  /api/v1/webhooks
POST /api/v1/webhooks                   # Create an outbound webhook
DELETE /api/v1/webhooks/{id}
POST /api/v1/webhooks/{id}/test
```

### 13.8 LP onboarding

```
POST /api/v1/lp/exchange-invite         # body: { invite_token, machine_id }
                                         # Returns long-lived user API key
GET  /api/v1/lp/policy-snippet          # LP pulls a hash-cached policy summary applicable to the machine
POST /api/v1/repo/bind                  # body: { remote_url, head_sha, machine_id }
                                         # Returns a signed binding token
GET  /api/v1/lp/version-check
```

---

## 14. Recommended tech stack

| Layer | Choice | Rationale |
|---|---|---|
| Gateway primary language | **Go 1.22+** | Single binary, concurrency model, mature KMS/AWS/GCP SDKs, native cel-go |
| LP primary language | **Go** (same as GW) | Cross-platform single binary; reuse transformer / scan / wire packages |
| HTTP routing | `chi/v5` | Lightweight, composable, middleware-friendly |
| Streaming | `net/http` + `r/w.Flusher` | No additional SSE library |
| Policy expression | `cel-go` | YAML + CEL is the industry de-facto standard |
| Config hot reload | `fsnotify` + atomic double-buffer | |
| Secret scan | `gitleaks` ruleset fork (Go-native) + `detect-secrets` (gRPC sidecar, Python) | Direct gitleaks Go calls; detect-secrets has broader coverage |
| Tokenizer | `tiktoken-go` (OpenAI) + `anthropic-tokenizer` (Anthropic) | |
| Provider SDK | Official Anthropic / OpenAI SDKs; others via OpenAI-compatible HTTP | |
| Postgres driver | `pgx/v5` | Performance, connection pooling |
| TimescaleDB | Postgres extension | Same instance as the main DB |
| Object storage | S3-compatible (aws-sdk-go-v2 s3) | MinIO / AWS S3 / GCS (via the S3-compatible layer) |
| KMS abstraction | aws-sdk-go-v2 kms / gcp kms / vault api / local file | Unified interface |
| Circuit breaker | `sony/gobreaker` | |
| Rate limit | `golang.org/x/time/rate` | In-memory token bucket |
| Logging | `zerolog` or `slog` (stdlib 1.22+) | Structured logs |
| Tracing | OpenTelemetry SDK + jaeger/otlp | Optional, BYO at the enterprise |
| Metrics | Prometheus client_golang | `/metrics` endpoint |
| Migration | `golang-migrate/migrate` | Auto-runs at startup |
| Dashboard | **Next.js 14 (app router) + Tailwind + shadcn/ui** | Static export embedded in the GW binary (`embed.FS`) |
| Tests | `go test` + testcontainers-go (Postgres + MinIO) | |
| Local dev | docker-compose (gw + postgres+ts + minio + ollama optional) | |
| Container | Distroless static | Minimal single-binary image |

> Alternative: if the team has materially stronger TypeScript experience (CCR fork background), the LP could be in TS (keep single-process dual endpoints + Node fastify); the GW is still strongly recommended to be Go (multi-process streaming + KMS / object storage + performance).

---

## 15. MVP directory structure

```
agentgate/
├── README.md
├── LICENSE
├── go.mod
├── go.sum
├── Makefile
├── docker-compose.yaml                # Local dev: gw + postgres-ts + minio
├── Dockerfile
├── docs/
│   └── architecture/
│       ├── SYSTEM-DESIGN.md           # this file
│       ├── envelope-schema.md
│       ├── policy-cookbook.md
│       └── threat-model.md
│
├── cmd/
│   ├── aicg-gw/
│   │   └── main.go                    # gateway entry
│   └── aicg-lp/
│       └── main.go                    # local proxy entry
│
├── internal/
│   ├── shared/                        # shared between GW + LP
│   │   ├── envelope/                  # Metadata envelope schema
│   │   ├── ir/                        # Anthropic IR types
│   │   ├── transformer/               # IR ↔ provider wire
│   │   │   ├── anthropic.go
│   │   │   ├── openai.go
│   │   │   └── degraded.go
│   │   ├── tokenizer/
│   │   ├── scan/                      # gitleaks rules wrapper + redact engine
│   │   ├── wire/                      # SSE helpers, JSON streaming
│   │   ├── secretref/                 # env://, file://, vault:// resolution
│   │   └── version/
│   │
│   ├── lp/                            # local proxy only
│   │   ├── server/
│   │   │   ├── anthropic.go           # /anthropic/* handler
│   │   │   ├── openai.go              # /openai/* handler
│   │   │   └── meta.go                # /_aicg/* handler
│   │   ├── tagger/                    # heuristic v0
│   │   ├── session/
│   │   ├── repobinder/
│   │   ├── gwclient/                  # mTLS, retry, timeout
│   │   ├── prescan/
│   │   ├── localconfig/               # ~/.aicg/*
│   │   └── cli/
│   │       ├── login.go
│   │       ├── start.go
│   │       ├── status.go
│   │       ├── bind.go
│   │       ├── doctor.go
│   │       └── env.go
│   │
│   └── gw/                            # gateway only
│       ├── edge/
│       │   ├── tls.go
│       │   ├── ratelimit.go
│       │   └── tracing.go
│       ├── auth/
│       │   ├── apikey.go
│       │   ├── rbac.go
│       │   └── invites.go
│       ├── repobinding/
│       ├── safety/
│       │   ├── input_scanner.go
│       │   ├── output_scanner.go
│       │   └── redactor.go
│       ├── classifier/                # server-side reclassifier
│       │   └── server_reclassifier.go
│       ├── policy/
│       │   ├── loader.go              # YAML + fsnotify
│       │   ├── engine.go              # CEL decisions
│       │   ├── conflict.go            # priority + deny-overrides
│       │   └── decision.go
│       ├── routing/
│       │   ├── pools.go
│       │   ├── selector.go
│       │   ├── breaker.go
│       │   └── execute.go
│       ├── provider/
│       │   ├── adapter.go             # interface
│       │   ├── anthropic.go
│       │   ├── anthropic_bedrock.go
│       │   ├── openai.go
│       │   ├── azure_openai.go
│       │   ├── openrouter.go
│       │   ├── litellm.go
│       │   └── openai_compat.go       # ollama / vllm / tgi
│       ├── budget/
│       ├── cost/
│       │   ├── pricing.go             # Pricing YAML loader
│       │   ├── extractor.go
│       │   └── estimator.go
│       ├── audit/
│       │   ├── writer.go
│       │   └── chain.go               # Hash chain periodic job
│       ├── rawstore/
│       │   ├── store.go
│       │   ├── kms.go                 # KMS interface
│       │   ├── kms_aws.go
│       │   ├── kms_gcp.go
│       │   ├── kms_vault.go
│       │   ├── kms_localfile.go
│       │   └── gc_worker.go
│       ├── webhook/
│       ├── approval/
│       ├── dashboard/
│       │   ├── api.go                 # REST API routes
│       │   ├── sse.go
│       │   └── ui_embed.go            # embed.FS static assets
│       ├── db/
│       │   ├── migrations/
│       │   ├── postgres.go
│       │   └── timescale.go
│       └── server/                    # main wiring
│           ├── server.go
│           ├── ingress_pipeline.go
│           ├── egress_pipeline.go
│           └── handlers.go
│
├── configs/
│   ├── policies/
│   │   └── main.yaml
│   ├── pools.yaml
│   ├── budgets.yaml
│   ├── identity/
│   │   ├── users.yaml                 # user_id, email, team_id
│   │   ├── teams.yaml
│   │   └── repos.yaml                 # repo_id, owners, sensitivity
│   ├── webhooks.yaml
│   └── pricing.yaml
│
├── scripts/
│   ├── dev/
│   ├── load-test/
│   └── policy-lint/
│
├── tests/
│   ├── e2e/
│   ├── conformance/
│   │   ├── claude_code/               # Spin up a Claude Code simulator and run end-to-end
│   │   └── openai_clients/            # cursor/aider style
│   └── fixtures/
│
└── ui/                                # Next.js dashboard
    ├── app/
    ├── components/
    ├── lib/
    └── package.json
```

---

## 16. Key interface definitions

### 16.1 LP↔GW interface (ABNF abbreviated) [v0.2.1 envelope endpoint]

```
;; HTTP request: POST /v1/agent/forward
Authorization        = "Bearer" SP user-api-key       ; user-api-key = 256-bit base64
X-AICG-LP-Version    = semver
X-AICG-Session-Id    = 64HEXDIG
X-AICG-Repo-Binding  = base64( ed25519-signed CBOR { repo_id, machine_id, exp } )
Accept               = "text/event-stream" / "application/json"

;; HTTP request body
body                 = JSON {
                         "envelope": AICGEnvelope,
                         "wire": {
                           "protocol": "anthropic_messages" / "openai_chat_completions",
                           "stream":   bool,
                           "body":     <upstream-original-body>
                         }
                       }
```

### 16.2 PolicyEngine

```go
package policy

type Engine interface {
    Decide(ctx context.Context, in DecideInput) (Decision, error)
    Reload(ctx context.Context) error
    CurrentVersion() string
}

type DecideInput struct {
    Envelope     envelope.AICGEnvelope
    ServerClass  classifier.Output         // task_type, complexity, sensitivity (server-trusted)
    User         auth.User
    Team         auth.Team
    Repo         auth.Repo
    Budget       budget.Snapshot
    TraceID      string                    // used to derive rand() deterministically
}

type Decision struct {
    PrimaryAction     string                // [v0.2] single-pick: allow|block|route|route_to_private_model|require_approval
    Modifiers         []string              // [v0.2] redact|escalate_to_strong_model
    SideEffects       []string              // [v0.2] shadow_eval|log_only
    ModelPool         string
    ShadowPool        string
    Redactions        []scan.Redaction
    Reasons           []string              // hit rule_ids
    Explanation       string                // human readable

    // [v0.2.3] Endpoint constraints — RoutingEngine uses these to filter candidate members
    RequiredTrustTier     string            // "" | "vendor" | "partner" | "private"
    RequiredDataResidency []string          // allowed set; empty = unrestricted
    RequiredCapabilities  []string          // required supports.* keys

    DegradedFeatures  []string              // populated during routing
    RequireApprovalID string
}
```

### 16.3 RoutingEngine

```go
package routing

type Engine interface {
    // [v0.2.3] BuildChain filters the registry by Decision's RequiredTrustTier / DataResidency / Capabilities
    BuildChain(pool string, decision policy.Decision, traceID string, attemptNo int) ([]Member, error)
    Execute(ctx context.Context, req ir.Request, chain []Member, traceID string) (Result, error)
    ExecuteStream(ctx context.Context, req ir.Request, chain []Member, traceID string) (StreamResult, error)
}

// [v0.2.3] Error: no candidate after constraint filtering
var ErrNoCandidate = errors.New("routing: no candidate after constraint filter")

type Result struct {
    Response       *ir.Response
    Member         Member
    AttemptNo      int
    UsedFallbacks  []FallbackHop
    Latency        time.Duration
    Degraded       []string
}
```

### 16.4 ProviderAdapter

See §10.

### 16.5 RawStore

```go
package rawstore

type Store interface {
    Put(ctx context.Context, in PutInput) (PutOutput, error)
    Get(ctx context.Context, traceID string, role auth.Role, purpose string, breakGlass bool) (Plaintext, error)
    Delete(ctx context.Context, traceID string) error
}

type PutInput struct {
    TraceID         string
    TenantID, UserID, TeamID, RepoID string
    Sensitivity     string
    Plaintext       []byte                 // canonical_json(request+response+events)
    StoragePolicy   string                 // 'full', 'redacted_only', 'disabled'
    Redactions      []scan.Redaction
    RetentionDays   int
}
```

### 16.6 KMS

See §11.2.

### 16.7 Audit

```go
package audit

type Writer interface {
    Append(ctx context.Context, ev Event) error
}

type ChainSigner interface {
    SealDay(ctx context.Context, date time.Time, tenantID string) (Root, error)
    Verify(ctx context.Context, date time.Time, tenantID string) (bool, Root, error)
}
```

### 16.8 Budget [v0.2 Reserve/Commit/Release]

```go
package budget

type ReservationID string

type Service interface {
    // Query current budget state during decision (includes the reserved portion)
    Check(ctx context.Context, q Query) (Snapshot, error)

    // Reserve before routing; over hard cap returns ErrHardCapExceeded
    Reserve(ctx context.Context, q Query, estimatedCents int) (ReservationID, error)

    // On upstream success → replace estimated with actualCents; delta auto-released
    Commit(ctx context.Context, id ReservationID, actualCents int) error

    // On upstream failure / client disconnect → full release
    Release(ctx context.Context, id ReservationID) error
}

type Snapshot struct {
    TeamMonthlyUsedCents     int   // committed
    TeamMonthlyReservedCents int   // outstanding reservations
    TeamMonthlyCapCents      int
    UserDailyUsedCents       int
    UserDailyReservedCents   int
    UserDailyCapCents        int
    SoftHit                  bool  // (used + reserved) >= 80% cap
    HardHit                  bool  // (used + reserved) >= 100% cap
}

// Background settler interface
type Settler interface {
    // Periodically cleans up Reservations that have not been committed/released for longer than (default 10min) → Release + alert
    SweepExpired(ctx context.Context, ttl time.Duration) (releasedCount int, err error)
}
```

---

## 17. Key risks and mitigations

| Risk | Impact | Mitigation |
|---|---|---|
| **Anthropic policy risk** (unauthorized wire compatibility) | Legal / commercial bans | Do not replace the Anthropic API key; GW is enterprise self-hosted with the enterprise-owned API key; traffic does not leave the enterprise boundary (except for legitimate upstream calls) |
| **Enterprise reluctance to install a client** | Onboarding friction | Single-binary LP + brew/npm/scoop channels + `aicg doctor` one-shot self-check; provide zero-touch deploy mode (admin pushes `~/.aicg/credentials` via MDM) |
| **Untrusted client-side tags** | Security policy bypass | All security/routing decisions are based on the server reclassifier; client tags only act as a confidence prior |
| **Streaming mid-failure fallback distortion** | Poor client experience | MVP simply breaks the stream + LP auto-retries once; no seamless stitching |
| **Cross-provider feature loss (cache_control, thinking)** | Cost / effectiveness regression | `degraded_features[]` lands in `routing_event`; the dashboard highlights explicitly; admins can use policy to forbid cross-provider routing for specific task_types |
| **Object storage + KMS failure** | Blocks audit / raw writes | Raw writes are best-effort (`async` queue + bounded retry + DLQ); audit must succeed synchronously; KMS unavailable → fail-closed denies raw writes but requests still return normally |
| **gitleaks false-positive rate** | Bad developer experience | Client pre_scan uses loose rules (high sensitivity → hint only); the server uses strict rules (decision authority); `aicg policy decision/explain` for self-service investigation |
| **Policy reload misconfigured syntax** | Everyone gets 5xx | Reload-failure keeps the old version + alert webhook; the `aicg policyctl validate` CLI runs before admin push |
| **Provider key leakage** | Disaster | Secret reference schemes (`vault://`/`env://`/`file://`); a plaintext key in git is blocked by lint |
| **Streaming post-hoc scan missing detections** | Compliance risk | Dual-engine (gitleaks + detect-secrets); the output scanner carries a rule version + periodic replay of historical raw rescans; hits fire webhook + dashboard pin |
| **Hash chain bypassed (admin edits DB directly)** | Audit integrity | Daily root written to S3 + signed + optional forward-to external storage (write-only); platform_admin operations themselves write audit |
| **Multi-provider pricing inaccuracy** | Financial reconciliation drift | Provider usage fields are the authoritative source; local models use tokenizer estimation marked `cost_source=estimated`; monthly reconciliation with the provider's invoice landed in Phase 2 |
| **LP / GW version incompatibility** | Client crash | The envelope `schema_version` uses strict SemVer; GW supports the current and previous major; `aicg doctor` reports version skew |
| **MVP without SSO; admin user/team drift** | Cost attribution errors | `users.yaml` / `teams.yaml` in git; every change is a PR; a reconcile job periodically checks for "orphan traces" (no user/team mapping) |
| **CCR mindshare** | Users ask "why not CCR" | Documentation makes the differences explicit: enterprise governance, policy, audit, cost attribution, security; the OSS plan keeps the LP lightweight; developer experience aligned with CCR |
| **Conflict with existing CCR env vars** | Concurrent use anomalies | `aicg doctor` detects an already-occupied `ANTHROPIC_BASE_URL` and prompts; offers `aicg env --check-conflicts` |

---

## 18. Iteration roadmap [v0.2 P0/P1 tightened and re-sliced]

> Review #1: v0.1 packed 6–9 months of work into Phase 0 — unreachable. v0.2 re-tiers. Each phase must independently deliver value and serve as ROI justification for the next.

### Phase 0 — MVP (0–3 months) "cost observability"

**Core deliverables**

- LP: **Anthropic endpoint only** (`/anthropic/v1/messages` + `count_tokens`)
- LP: local config / repo auto-bind / `aicg login/start/status/doctor/bind-repo`
- LP: heuristic tagger v0
- GW: edge + auth (API key + RBAC) + repo_binding validation
- GW: server reclassifier (lightweight rules version)
- Policy engine: YAML + CEL + hot reload; only the `route` action (**no block/redact/require_approval/shadow_eval**)
- Routing engine: 4 pools + Anthropic and 1 OpenAI-compatible adapter (pick one of Ollama/vLLM)
- **`provider_endpoints` registry** validation (startup + reload reject unregistered endpoint_id)
- Three Postgres tables: `audit_event` / `cost_event` / `routing_event` (**TimescaleDB postponed to P1**)
- Pricing YAML + cost extractor (based on provider usage fields)
- Budget service: `Check` implemented; `Reserve/Commit/Release` interfaces land but cap is soft-warn only (non-blocking)
- CLI reports: `aicg stats --by team|user|repo|model --from --to`

**Out of scope**: dashboard UI, webhook, KMS, object storage, security scanning, approval, shadow_eval, TimescaleDB, additional providers (Bedrock/Azure/OpenRouter/LiteLLM, etc.), hash chain.

**Exit criteria**: one internal dogfood team is able to produce a weekly cost report + replayable decisions (trace_id replay). SLO: p95 GW latency < 150ms (upstream excluded); availability 99%.

### Phase 1 — Team Gateway Beta (3–6 months) "governance executable"

- LP: second endpoint (`/openai/v1/chat/completions`) + transformer pipeline across wires
- Routing engine: fallback chain + circuit breaker
- Budget: Reserve/Commit/Release **truly enforces the hard cap**
- TimescaleDB migration + continuous aggregates
- **Input pre-scan v0 (gitleaks single engine) + policy `block` / `redact` actions ship**
- Policy engine: `require_approval` action + approval queue + generic webhook notification
- Minimal dashboard: cost views + routing recent + approval queue
- Outbound webhooks (alert / approval / violation) with HMAC + URL allowlist

**Exit criteria**: ≥3 teams onboard with at least 1 enterprise customer pilot; core policy enforcement is demoable (block / redact / approval); MTTR < 30 min (policy-error rollback).

### Phase 2 — Raw capture & eval-first foundation (6–9 months)

- Raw store + KMS abstraction (AWS KMS / local file KEK first); default still `metadata_only`, opt-in per repo
- access_audit + three-layer RBAC (developer / team_admin / platform_admin)
- shadow_eval loop: dual emit to a table + simple scorecard
- More providers: OpenRouter / LiteLLM / Azure OpenAI
- Policy preview (dashboard): decision simulation + decision diff
- LP binary signature verification + self-check upgrade hints

**Exit criteria**: raw capture usable in production at ≥1 enterprise; shadow_eval data usable to drive P3 recommendations.

### Phase 3 — Enterprise hardening (9–12 months)

- Audit hash chain + daily-root dual write
- Second scan engine (detect-secrets sidecar)
- Anthropic Bedrock / private-domain endpoints / `private_strong` pool truly online
- OIDC device-code (SSO entry point)
- Post-hoc output scan (post-stream + alert)
- KMS multi-provider full support (GCP KMS, Vault Transit)
- LP platform-native key management (macOS Keychain / Windows DPAPI)

### Phase 4 — Enterprise control plane (12 months+)

- Full SSO / SAML / SCIM
- SOC2-ready immutable audit (WORM object storage + third-party notarization)
- Multi-region / data residency
- BYO KMS + customer-managed keys
- Enterprise RBAC (ABAC)
- Multi-tenant SaaS shape
- Deep IDE integration (VS Code / JetBrains)
- Private inference cluster orchestration
- DLP hardening (third-party integration)

### Trade-off log

> Why does P1 bring back input pre-scan + block/redact instead of deferring to P2 as suggested by review?
>
> AgentGate's core narrative is "governance". Pure cost routing in P0 already overlaps with LiteLLM++/CCR forks; P1 must demonstrate "governance" to anchor enterprise sales mindshare. pre-scan v0 with gitleaks single engine is cost-controlled (≤2 weeks of work).
>
> Why does raw store + KMS land in P2 instead of all in P0 as v0.1 had it?
>
> After v0.2 defaults to `metadata_only` (review #9), P0/P1 no longer needs KMS or object storage. Raw is high-value, low-urgency — deferring frees engineering capacity for the P0 core closed loop.

---

## 19. Hardcode vs abstraction boundary

> Principle: **short-lived / industry-standard → hardcode; replaceable / compliance-driven / customer-specific → abstract.**

### Should hardcode (avoid over-design)

| Item | Reason |
|---|---|
| Internal IR = Anthropic Messages API | The only controllable complexity; multi-IR is a disaster |
| Client wires = `/anthropic/*` + `/openai/*` two endpoints | Industry de-facto; no third worth adding |
| 12 task-type enumerations | Already tightly coupled with policy expression; extend via `custom_tags` |
| Policy DSL = YAML + CEL | One expression layer is enough |
| Policy conflict algorithm = priority + deny-overrides + modifier | Do not introduce a second semantics |
| Audit field fixed schema (hash chain input) | Any field change breaks the historical chain |
| Storage object key layout | Parsing / GC / migration tools all depend on it |
| trace_id = UUIDv7 | Time-ordered + unique + industry-converging |
| session_id algorithm = `sha256(agent_pid + start_ts + repo)` | Client can reproduce independently |
| CLI subcommand names (`aicg login/start/status/bind-repo/doctor`) | User recall |
| envelope `schema_version` field position | Cross-version compatibility foundation |
| 4 model pools (cheap/standard/strong/private_strong) | Users already bound to policy templates |
| KMS envelope encryption (DEK + KEK) | The only evolvable + compliant scheme |

### Must abstract (interface first)

| Item | Abstraction | Replacement scenarios |
|---|---|---|
| Provider adapter | `provider.Adapter` interface | New provider onboarding |
| Transformer | Per-direction transformer interface | Cross-provider feature mapping evolution |
| KMS | `rawstore.KMS` interface | AWS KMS / GCP KMS / Vault Transit / local |
| Object storage | `rawstore.Backend` (S3-compatible) | S3 / GCS / MinIO / Azure Blob |
| Secret reference | `secretref.Resolver` (`env://` `file://` `vault://`) | Add AWS SM, GCP SM |
| Identity source | `auth.UserDirectory` interface (MVP=YAML, Phase 1=OIDC, Phase 3=SCIM) | Without breaking callers |
| Approval channel | `approval.Notifier` interface | webhook / slack / email / pagerduty |
| Scan engine | `scan.Engine` interface | gitleaks / detect-secrets / Nightfall / Lakera |
| Pricing source | `cost.Pricer` interface | YAML table / provider billing API / third party |
| Tokenizer | `tokenizer.Tokenizer` per provider | tiktoken / anthropic / in-house |
| Webhook transport | `webhook.Transport` interface | http / kafka / sqs |
| Tenant resolver | `tenant.Resolver` interface (MVP single-tenant constant) | Future SaaS multi-tenant |
| Region locator | `region.Locator` interface (MVP single-region constant) | Future data residency |
| Server reclassifier | `classifier.Reclassifier` interface | Future ML classifier |
| Policy storage | `policy.Source` interface (MVP=fs/yaml) | DB / remote config center |
| Metrics emitter | OpenTelemetry / Prometheus interface | Not vendor-locked |

### Build now but do not abstract yet (YAGNI)

- **Multi-language SDKs**: the MVP only has the LP as a single client; do not build client SDKs
- **Plugin / extension mechanism**: the MVP's built-in scanner is enough; do not open up external plugins
- **Multi-policy-file / policy merge**: the MVP uses a single `policies/main.yaml`; do not introduce namespacing
- **Approval workflow DSL**: single-step approval is enough for the MVP; no multi-stage / delegation
- **Cross-cluster synchronization**: the MVP is single-instance; no leader election / consensus

---

## 20. Implementation Readiness Checklist (P0) [v0.2.2 new]

> The minimum demoable set for P0. Each item references a file path and contract anchor for easy task pickup. **All 22 items green to declare P0 done.**

### 20.1 Repo and infrastructure

- [ ] **R-1** Go module init (`go mod init agentgate`); `Makefile` + `Dockerfile` (distroless static)
- [ ] **R-2** `docker-compose.yaml`: gw + Postgres (**without** TimescaleDB / MinIO / KMS)
- [ ] **R-3** `golang-migrate` integrated; `internal/gw/db/migrations/` contains schema 0001 (plain Postgres, see §11.3 §12.1 §12.2 §12.3 §12.4 §12.5)

### 20.2 GW schema & config

- [ ] **R-4** Postgres tables: `raw_record` (with CHECK constraint) / `audit_event` (self_hash NULL) / `cost_event` / `routing_event` / `approval_request` / `budget_reservation` / `raw_access_audit` / `audit_chain_root`
- [ ] **R-5** `configs/pools.yaml` includes a `provider_endpoints` registry (with trust_tier/data_residency/supports); startup validates that every pool member.endpoint_id is in the registry
- [ ] **R-6** `configs/policies/main.yaml` only contains `route` / `allow` style rules; `block`/`redact`/`require_approval` go to P1
- [ ] **R-7** `configs/pricing.yaml` includes at least Anthropic flagship models + 1 openai_compat (cost can be 0)

### 20.3 GW handlers & pipeline

- [ ] **R-8** `POST /v1/agent/forward` (§5.4.1): parse envelope + wire; full ingress pipeline (§4 steps 9-23)
- [ ] **R-9** Auth middleware: API key bcrypt hash table + RBAC + setup_token bootstrap (§13.1)
- [ ] **R-10** Repo binding validation: ed25519 signature verify + machine_id association (§16.1)
- [ ] **R-11** Server reclassifier (lite): heuristic recomputation of task_type based on file_paths / language / diff_summary
- [ ] **R-12** Policy engine: cel-go compilation + three-slot merge (§7 §8); fsnotify hot reload (reload-failure keeps the old version + alert)
- [ ] **R-13** Routing engine: trace_id-derived ChaCha8 seed + weighted pick (§9.2); write `routing_event.seed`
- [ ] **R-14** Provider adapter interface landed (§10) + two adapters: `anthropic`, `openai_compat`
- [ ] **R-15** Egress: streaming SSE passthrough + inject `aicg.usage` at stream end (§5.4.3); non-streaming cost via header
- [ ] **R-16** Budget service: `Reserve / Commit / Release` interfaces land (P0 soft-warn only, non-blocking); background settler scans timeouts every minute

### 20.4 LP

- [ ] **R-17** `cmd/aicg-lp` single binary: only exposes `/anthropic/v1/messages` + `/_aicg/*` (the OpenAI endpoint goes to P1)
- [ ] **R-18** CLI subcommands: `login --setup` / `login --invite` / `start` / `status` / `bind-repo` / `doctor` / `env` / `version`
- [ ] **R-19** Tagger v0: file_paths/language/diff_summary heuristics (no secret pre-scan; that lands in P1)
- [ ] **R-20** GW client: optional mTLS / retry budget / first-token-flush state machine (§5.4.4)
- [ ] **R-21** `aicg.usage` SSE event parsing + local ledger (`~/.aicg/traces.db` SQLite)

### 20.5 Tests

- [x] **R-22** Conformance test: spin up a mock Anthropic upstream + Claude Code simulator; run an end-to-end `code_edit` streaming request; assert (a) `cost_event` written; (b) `routing_event.seed` non-empty; (c) `audit_event` written; (d) `aicg status` reports this trace; (e) `routingctl replay <trace_id>` matches the original decision (evidence: tests/conformance/claude_code/e2e_dbbacked_test.go::TestE2EStreamingCodeEditDBBacked; when Docker is unavailable, fall back to the in-memory code-layer smoke test TestE2EStreamingCodeEdit)

### 20.6 Exit criteria (aligned with §18 Phase 0 exit criteria)

- 1 internal dogfood team onboard for at least 1 week
- p95 GW latency (upstream excluded) < 150ms
- Availability 99% / 1 week
- Weekly report (`aicg stats --by team --from 7d`) output
- One full incident-replay drill (rebuild the path with `routingctl replay`)

---

## Appendix A: pricing-table schema example

```yaml
# pricing.yaml
version: 1
prices:
  - provider: anthropic
    model: claude-opus-4-7
    input_cents_per_mtok: 1500           # $15 / 1M tokens
    output_cents_per_mtok: 7500
    cache_write_cents_per_mtok: 1875
    cache_read_cents_per_mtok: 150
  - provider: anthropic
    model: claude-sonnet-4-6
    input_cents_per_mtok: 300
    output_cents_per_mtok: 1500
  - provider: openai
    model: gpt-4o
    input_cents_per_mtok: 250
    output_cents_per_mtok: 1000
  - provider: openrouter
    model: deepseek/deepseek-chat
    input_cents_per_mtok: 27
    output_cents_per_mtok: 110
  - provider: openai_compat
    endpoint: ollama-cluster
    model: qwen2.5-coder:7b
    input_cents_per_mtok: 0
    output_cents_per_mtok: 0
    cost_source_default: estimated
```

## Appendix B: typical Decision Explain output

```json
GET /api/v1/policy/decision/explain
body:
{
  "envelope": { ... omitted ... },
  "server_class": { "task_type": "code_edit", "data_sensitivity": "medium" }
}

response:
{
  "decision": {
    "action": "route",
    "modifiers": ["shadow_eval"],
    "model_pool": "standard",
    "shadow_pool": "strong",
    "reasons": ["P-ROUTE-002", "P-EVAL-001"]
  },
  "checked_rules": [
    {"id": "P-SEC-001", "matched": false, "trace": "envelope.repo.repo_id not in restricted_repos"},
    {"id": "P-ROUTE-002", "matched": true},
    {"id": "P-EVAL-001", "matched": true, "trace": "rand()=0.031 < 0.05"}
  ]
}
```

## Appendix C: default-policy routing coverage self-check

| task_type | data_sensitivity | Default pool | Covering rule |
|---|---|---|---|
| summary | low/med | cheap | P-ROUTE-001 |
| test_output | low/med | cheap | P-ROUTE-001 |
| repo_search | low/med | cheap | P-ROUTE-001 |
| file_reading | low | cheap | P-ROUTE-001 + override |
| file_reading | med | standard | P-ROUTE-002 |
| simple_edit | * | standard | P-ROUTE-002 |
| code_edit | * | standard | P-ROUTE-002 |
| planning | low/med | standard | P-ROUTE-002 |
| planning | high | private_strong | P-ROUTE-004 |
| architecture | * | strong | P-ROUTE-003 |
| debug | * | strong | P-ROUTE-003 |
| security_review | * | strong | P-ROUTE-003 |
| review | * | standard | P-ROUTE-002 |
| unknown | * | standard | defaults.on_no_match |
| any | high or restricted_repo | private_strong | P-ROUTE-004 |
| any | secret hit | block / redact | P-SEC-001 / P-SEC-002 |

---

**End of design v0.1.**
