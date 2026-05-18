# Architecture Docs Changelog

## v0.2.4 — 2026-05-13 (CLI examples sync)

The implementation-side `aicg login` flag parser only accepts space-separated values; this revision only syncs the copy-pastable CLI examples in the architecture docs without changing any architectural decision.

| # | Topic | Revision |
|---|---|---|
| 1 | Login flag examples | The `aicg login --invite=<token>` / `--setup=<token>` form in §1 R6, §5.2, §13.8 changed to the space-separated form |

## v0.2.3 — 2026-05-04 (Interface-semantics alignment + P0 boundary cleanup)

Fourth review: 5 items. Item 1 is an actual architectural revision (policy / routing responsibility split); the rest are documentation sync.

| # | Topic | Revision |
|---|---|---|
| 1 | Policy must not enumerate routing candidates; switched to outputting constraints | Decision gains three fields `Required{TrustTier,DataResidency,Capabilities}`; RoutingEngine filters endpoints by constraints in BuildChain; §9.1 "CEL references endpoints" rewritten as "constraint output"; §9.2 SelectChain gains a `constraints` parameter |
| 2 | P0 budget soft warn should not fire a webhook | §9.5 changed: in P0, soft warn only writes `audit_event` + CLI stats; the webhook fires from P1 onward (consistent with §18) |
| 3 | Threat-model hash-chain old wording | TB-5 drops "detected by hash chain"; Top-3 row uses phased wording |
| 4 | Provider-key KMS phase clarification | Made explicit: the secret resolver (`env://` / `file://` / `vault://`) is independent of the raw-store KMS; P0 supports the first two — `vault://` requires the enterprise to already operate Vault; P0 residual risk is listed explicitly in threat-model |
| 5 | Cookbook §14 title [v0.2] | Updated to [v0.2.3] |

## v0.2.2 — 2026-05-04 (Cross-document sync + implementation checklist)

Third review: 8 cross-document carry-overs + 3 optional polish items. All adopted.

| # | Topic | Revision |
|---|---|---|
| 1 | §9.4 old streaming-failure semantics | Changed to reference §5.4.4 to avoid two parallel formulations |
| 2 | §9.5 duplicate paragraphs | Removed the duplicate estimated / settlement block |
| 3 | Cookbook §14 checklist old allowlist | Updated to "endpoint_id ∈ provider_endpoints registry + codeowners approval" |
| 4 | Threat-model TB-10 + mitigation mapping old allowlist | Synced to `provider_endpoints` registry; attack surface rephrased as "PR adds / tampers with endpoint_id or URL" |
| 5 | Threat-model hash-chain phase wording | Updated to P0/P1 residual "no cryptographic tamper evidence"; hash chain / WORM only in P3 |
| 6 | Threat-model TB-4 envelope header cap | Updated to body-wrapper size limit (`MaxRequestBodySize` + `MaxEnvelopeBytes`) |
| 7 | Threat-model Top-N "raw fully retained for traceability" | Updated to "metadata + audit are traceable; raw traceability holds only after P2+ opt-in" |
| 8 | Cookbook trailing version number | v0.1 → v0.2.1 |
| A | **Added §20 Implementation Readiness Checklist** | 22-item list of schemas/handlers/CLI/tests required for P0 |
| B | **`provider_endpoints` extended fields** | Added `data_residency` / `trust_tier` / `supports_*`; policies now reference the registry directly instead of variables |
| C | **`aicg.usage` / `aicg.error` SSE event schema formalized** | §5.4.3 adds a strict JSON Schema definition + LP passthrough rules + curl/debug mode documentation |

## v0.2.1 — 2026-05-04 (Consistency sweep)

The second review found that the v0.2 revisions landed only in the new sections, while several v0.1 leftover sections clashed with the new decisions. This sweep covers 10 items.

| # | Topic | Revision | Sections |
|---|---|---|---|
| 1 | §1 decision table A2 / §4 request lifecycle residual v0.1 wording | Fully rewritten per v0.2: envelope endpoint / three-slot Decision / budget reserve / raw defaults to `metadata_only` | §1 / §4 |
| 2 | `raw_record` schema clashes with `metadata_only` | `object_uri / object_size / kek_id` made nullable; added a `mode` constraint matrix | §11.3 |
| 3 | P0 does not run TimescaleDB but schema declared hypertable directly | Annotated "P0 = plain Postgres tables + plain indexes; P1 migration converts to hypertable / continuous aggregate" | §12.2 §12.3 |
| 4 | Endpoint allowlist cannot align with pool member endpoint fields | Introduced a `provider_endpoints` registry (`endpoint_id → url`); pools reference `endpoint_id`; allowlist validates `endpoint_id` and the resolved URL | §9.1 |
| 5 | Streaming-failure "passthrough 503" is not feasible | Made explicit: the LP may retry internally only **before** flushing the first token to the agent; once flushed, only stream termination is allowed — status cannot be changed | §5.4.4 |
| 6 | §10 "MVP 7 adapters" inconsistent with Phase 0 | Title changed to "Target adapter list"; added a P0/P1/P2/P3 matrix | §10 |
| 7 | Budget Reserve semantics did not differentiate phases | Made explicit — P0: soft-warn only; P1: true hard-cap enforcement + approval hook | §9.5 |
| 8 | Audit hash chain misaligned with Phase 0 scope | `audit_event.self_hash` made nullable; `prev_hash` / seal job marked P3; P0/P1 only append-only | §12.1 |
| 9 | §10 `Usage` struct duplicated | Removed duplicate block | §10 |
| 10 | §7.2 decision flow still used the old `Decision{action,...}` | Updated to the v0.2 three-slot form | §7.2 |

## v0.2 — 2026-05-04

Based on the 10 revisions from the first architecture review. The original v0.1 document remains as the 8–12 month target architecture; v0.2 compresses it into an executable 0–3 month MVP plan and fixes several interface / semantic debts.

### Scope

- `SYSTEM-DESIGN.md` v0.1 → v0.2
- `policy-cookbook.md` v0.1 → v0.2
- `threat-model.md` v0.1 → v0.2

### Revisions

| # | Topic | Revision summary | Affected sections |
|---|---|---|---|
| 1 | Phase 0 scope tightening | Rewrote the Phase 0/1/2/3 split; P0 contracts to LP Anthropic endpoint + dual adapter + three tables + routing + endpoint allowlist; TimescaleDB / security scanning / dashboard / KMS / hash chain all pushed down to P1+ | SYSTEM-DESIGN §18 |
| 2 | Streaming-response cost header dropped | HTTP/1.1 SSE responses cannot append a normal header after the body begins. Streaming no longer promises `X-AICG-Cost-Cents`; instead, a custom event `event: aicg.usage` is emitted before SSE termination; clients can also call `GET /api/v1/cost/breakdown?trace_id=`. The non-streaming response still carries the header | SYSTEM-DESIGN §5.4 |
| 3 | Decision object unified | Standardized across the document on the three-slot model `primary_action + modifiers[] + side_effects[]`, eliminating the three inconsistent semantics in §7.3 / §8 / §16.2 | SYSTEM-DESIGN §7.3 §8 §16.2; cookbook entire document |
| 4 | Budget reserve / settle | Introduced the three-stage semantics `Reserve(estimated) → Commit(actual) / Release()` and the `budget_reservation` table; the hard cap no longer relies on after-the-fact `Consume`, avoiding high-concurrency leakage | SYSTEM-DESIGN §9.5 §12.5 §16.8 |
| 5 | Routing selection is replayable | weighted-random selection seed = `sha256(trace_id : pool : attempt_no)`; the `routing_event` table gains a `seed` column | SYSTEM-DESIGN §9.2 §12.3 |
| 6 | IR decoupled from the Anthropic SDK | Own `ir.Message / ir.ContentBlock / ir.Tool / ir.ToolUse / ir.ToolResult / ir.CacheControlSpec / ir.ThinkingSpec`; Anthropic SDK types only appear inside the Anthropic adapter | SYSTEM-DESIGN §10 |
| 7 | LP→GW switched to a dedicated envelope endpoint | LP→GW no longer passes through wire + header envelope; instead, `POST /v1/agent/forward` with body `{envelope, wire:{protocol, body}}`, avoiding the 8KB header limit and the "half-transparent" contradiction. The LP still exposes `/anthropic/v1/messages` and `/openai/v1/chat/completions` internally | SYSTEM-DESIGN §5.4 §13.x |
| 8 | Auth single identity model | Removed dashboard email + password login; MVP is all API keys + RBAC roles; platform_admin remains an API key with a role; first-time startup is bootstrapped by a one-shot setup token | SYSTEM-DESIGN §13.1 §13.8; threat-model TB-9, RR-9 |
| 9 | Raw defaults to metadata_only | Default `storage_policy = metadata_only`; `redacted_only` / `full` require an explicit per-repo override; MVP P0 does not deploy KMS / object storage | SYSTEM-DESIGN §11.4; threat-model A1 |
| 10 | Endpoint allowlist landed in the main document | `pools.yaml` top-level `provider_endpoint_allowlist`; GW startup + reload validates that the `(provider, endpoint)` for every pool member appears in the list; adding an endpoint goes through a separate PR + security review; cookbook checklist updated | SYSTEM-DESIGN §9.1; cookbook §14 |

### Backward compatibility

v0.2 only touches the design document — no code changes (no code yet to break). Implementation drafts already started from v0.1 should be re-checked against the §Affected sections of this changelog.

### Decision sign-off

- Architecture owner: (pending)
- Security owner: (pending)

## v0.1 — 2026-05-03

Initial design draft. Decisions originate from three confirmation rounds (A/B/C/D/E/F/G/H series + Q1–Q20 + R1–R8). See SYSTEM-DESIGN.md §1 decision list for details.
