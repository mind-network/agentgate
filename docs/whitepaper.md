# AgentGate: An Enterprise AI Coding Gateway for Governed, Observable, and Cost-Aware Agent Traffic

## Abstract

AI coding agents are becoming part of everyday software delivery, but most enterprises still lack a practical control plane for how those agents access models, route sensitive code context, consume budget, and produce audit evidence. AgentGate is an enterprise AI coding gateway designed to sit between developer workstations and upstream AI providers. It combines a local proxy, a centralized gateway, policy-based routing, cost attribution, audit logging, and phased security controls into one deployable architecture.

The current Phase 0 implementation focuses on cost observability and operational dogfooding. It establishes the core local proxy and gateway path, authenticated traffic forwarding, repo binding, route-only policy evaluation, deterministic model routing, provider adapters, Postgres-backed audit, cost, and routing records, hot-reloadable configuration, and end-to-end conformance tests. Future phases extend this foundation into enforceable governance, raw capture with KMS, shadow evaluation, enterprise identity, immutable audit, and a broader control plane.

## 1. The Enterprise Problem

AI coding agents create a new category of infrastructure traffic. They carry source code, diffs, logs, test output, design context, credentials by accident, and sometimes customer-adjacent data. Direct provider usage gives developers speed, but it leaves platform teams with recurring gaps:

- No unified record of which team, repo, session, task, and model produced each request.
- Limited control over when requests should use public models, private endpoints, or stronger models.
- Weak cost attribution across users, teams, repos, and providers.
- Inconsistent policy enforcement across Claude Code, OpenAI-compatible tools, local models, and future IDE integrations.
- Insufficient auditability for security, compliance, incident replay, and budget review.
- Risky default handling of raw prompts and responses.

AgentGate addresses this by creating an enterprise-owned gateway path for coding-agent traffic while preserving a developer workflow that feels close to direct provider use.

## 2. Product Positioning

AgentGate is a CCR-like AI Coding Gateway for enterprises. It is not just a provider proxy and not just a cost dashboard. Its core value is a policy and observability layer purpose-built for coding agents.

The architecture has two primary components:

- **Local Proxy (LP)**: a developer-side process that exposes local Anthropic-compatible endpoints, enriches requests with metadata, manages repo binding, and forwards traffic to the enterprise gateway.
- **Gateway (GW)**: an enterprise-side service that authenticates users, validates repo bindings, reclassifies task hints, evaluates policy, selects provider endpoints, forwards requests, records audit, cost, and routing metadata, and exposes operational APIs.

This split lets developers keep using familiar tools while giving platform teams a central place to govern traffic.

## 3. Architecture Overview

AgentGate uses a dedicated LP-to-GW protocol instead of relying on ad hoc headers. The LP accepts tool-native wire formats locally, then sends a structured request to:

```http
POST /v1/agent/forward
```

The request contains:

- An **envelope** with client, identity, repo, session, and context metadata.
- A **wire payload** containing the original upstream protocol and request body.

The Gateway then runs the request through a staged pipeline:

1. API key authentication and RBAC.
2. Repo binding validation.
3. Server-side task reclassification.
4. Policy evaluation using YAML plus CEL.
5. Deterministic weighted routing using a trace-derived seed.
6. Provider adapter execution.
7. Streaming response relay.
8. Usage, routing, cost, raw metadata, and audit persistence.

A key design choice is separation between policy and routing. Policy emits constraints and routing intent; the routing engine resolves those constraints against the trusted `provider_endpoints` registry. This reduces the risk of policy rules directly selecting arbitrary physical URLs.

## 4. Policy and Routing Model

AgentGate's policy decision model is intentionally structured:

- `primary_action`
- `modifiers[]`
- `side_effects[]`

Phase 0 supports route-oriented behavior. Blocking, redaction, approval, and shadow evaluation are deliberately deferred to later phases so the MVP can prove the traffic path, cost attribution, deterministic replay, and operational reload loop first.

Routing is built around model pools and registered provider endpoints. Each endpoint carries properties such as wire protocol, vendor, trust tier, data residency, supported capabilities, URL, and key reference. Pools reference endpoint IDs rather than raw URLs. Startup and reload validation reject unknown endpoint IDs.

Routing uses a deterministic seed derived from `trace_id`, pool, and attempt number. That makes routing decisions replayable for incident analysis and debugging.

## 5. Phase 0 Implementation Status

The current P0 implementation establishes the core dogfoodable product path.

Implemented capabilities include:

- Go module, Makefile, Dockerfile, and Docker Compose foundation.
- Gateway and local proxy binaries.
- Postgres-backed migrations and persistence.
- API key bootstrap and bcrypt-hashed key storage.
- RBAC-aware gateway handlers.
- Repo binding flow.
- YAML configuration for pools, pricing, policies, users, teams, and repos.
- Hot reload with old-config retention on invalid updates.
- CEL-backed route policy engine.
- Deterministic routing and replay primitives.
- Anthropic and OpenAI-compatible provider adapter interfaces.
- Streaming SSE forwarding.
- AgentGate usage metadata injection through `aicg.usage` events.
- Postgres-backed `audit_event`, `cost_event`, `routing_event`, raw metadata, budget, and API key tables.
- Secret references through `env://` and `file://`.
- Operator and user runbooks.
- End-to-end tests with mock upstream providers.
- P0 performance validation for Gateway internal latency.

P0 should be understood as a cost-observability and internal dogfood release. Some fidelity improvements, especially exact provider-usage propagation into cost events, are explicitly tracked as follow-up work.

## 6. Security and Compliance Posture

AgentGate takes a phased security approach. The most important P0 posture is conservative raw-data handling: raw prompts and responses are not stored by default. P0 and P1 operate in `metadata_only` mode, which records request metadata without storing raw bodies.

P0 controls include:

- High-entropy API keys stored as hashes.
- RBAC-aware gateway access.
- Repo binding validation.
- Endpoint registry validation.
- TLS-oriented deployment guidance.
- Postgres-backed audit and routing records.
- Sensitive config change process expectations.
- Secret references instead of plaintext provider keys in YAML.

Known P0 residual risks are explicit:

- Provider keys using `env://` or `file://` enter process memory.
- Audit records are append-oriented but not yet cryptographically tamper-evident.
- Raw-store KMS, object storage, and break-glass access audit are deferred.
- Blocking, redaction, and approval controls are not yet active in P0.

This makes P0 suitable for internal dogfood and cost visibility, while later phases add stronger governance and compliance primitives.

## 7. Roadmap

### Phase 1: Team Gateway Beta

Phase 1 turns observability into enforceable governance. Planned work includes OpenAI-compatible LP endpoints, fallback chains, circuit breakers, TimescaleDB migration, hard budget caps, input pre-scan with gitleaks, `block` and `redact` policy actions, approval flows, webhooks, and a minimal dashboard for cost, routing, and approval queues.

### Phase 2: Raw Capture and Eval Foundation

Phase 2 introduces opt-in raw capture with KMS-backed storage, access audit, stronger RBAC for raw access, shadow evaluation, policy preview, more providers, and client binary integrity checks. Raw storage remains opt-in rather than a default behavior.

### Phase 3: Enterprise Hardening

Phase 3 focuses on deeper enterprise security: audit hash chains, daily root anchoring, a second scanning engine, private model pools, OIDC device-code login, post-hoc output scanning, broader KMS support, and native OS key management for LP credentials.

### Phase 4: Enterprise Control Plane

The long-term control plane adds SSO, SAML, SCIM, WORM-backed immutable audit, multi-region and data residency controls, BYO KMS, advanced RBAC and ABAC, SaaS multi-tenancy, IDE integrations, and private inference-cluster orchestration.

## 8. Why This Architecture Matters

AgentGate is designed around a practical sequence: first observe, then enforce, then harden. This avoids overloading the MVP with raw storage, KMS, dashboards, and enterprise identity before the core traffic path is reliable.

The design also avoids common proxy pitfalls:

- Provider URLs are governed through a registry, not scattered through policy.
- Routing decisions are replayable.
- Policy output is structured rather than a loose action string.
- Raw storage is not enabled by default.
- P0 uses ordinary Postgres before introducing TimescaleDB complexity.
- Gateway and LP responsibilities are separated cleanly.
- Provider adapters are abstracted, while core IR and routing semantics remain stable.

## Conclusion

AgentGate provides a foundation for enterprise-grade AI coding infrastructure. Its P0 release proves the core path: developers can route coding-agent traffic through a local proxy and enterprise gateway, while platform teams gain cost records, routing evidence, audit metadata, config reload, and deterministic replay.

The roadmap then expands that foundation into enforceable policy, raw-data governance, security scanning, approval workflows, enterprise identity, immutable audit, and a full AI coding control plane. The result is a system that lets organizations adopt AI coding agents without surrendering visibility, budget control, or governance.
