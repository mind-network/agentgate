# AgentGate Policy Cookbook

> Version: **v0.2.2** (2026-05-04)
> Companion docs: `SYSTEM-DESIGN.md` §7 Policy Engine, §8 Conflict Resolution, `CHANGELOG.md`.
> Audience: platform_admin, AI Platform team, security team.
> Purpose: recipes for the common policy YAML patterns, conventions, and pitfalls, so enterprises do not need to draft policies from scratch when onboarding.

## v0.2 revision summary

- **Three-slot model**: `action:` is single-pick; `modifiers:` and `side_effects:` are lists (v0.1 also used `modifiers`, but the concept was not aligned with the main document; v0.2 locks it down). See SYSTEM-DESIGN §7.3 / §8.
- The deployment checklist gains an endpoint-allowlist validation item.

---

## 1. Reading guide

Each recipe has the form:

```yaml
- id: <RULE-ID>
  description: <one-liner>
  priority: <int>
  when: |
    <CEL expression>
  action: <verb>           # single-pick from PrimaryAction column
  modifiers:               # optional, 0..N
    - <verb>
  side_effects:            # optional, 0..N
    - <verb>
  model_pool: <pool>       # when action == route / route_to_private_model
  shadow_pool: <pool>      # when side_effects include shadow_eval
  reasons: [<short tag>]   # required for block / redact / require_approval
```

**Three-slot verb classification** (memorize — wrong slot fails lint)

| Slot | Verbs |
|---|---|
| PrimaryAction (single-pick) | `allow`, `block`, `route`, `route_to_private_model`, `require_approval` |
| Modifiers (stackable) | `redact`, `escalate_to_strong_model` |
| SideEffects (stackable) | `shadow_eval`, `log_only` |

> **Backward-compatible shorthand**: the v0.1-era `action: redact` / `action: shadow_eval` / `action: log_only` / `action: escalate_to_strong_model` is auto-relocated to the right slot at lint time (PrimaryAction is then treated as default, supplied by another rule). New rules should use the canonical form directly.

Copy any block directly into the `rules:` list of `configs/policies/main.yaml` and edit fields as needed.

**Edit workflow (mandatory)**

1. Edit `configs/policies/main.yaml`; run `aicg policyctl validate` locally (parse + CEL compile + match simulation).
2. Push the PR to git; CI runs the same lint plus a set of fixture replays.
3. After merge, `aicg policyctl reload` (platform_admin only); the gateway hot-reloads via inotify.
4. Watch `dashboard/routing/recent` for `degraded_features` and `decision` hit rate over the next 30 minutes; rollback thresholds: 5xx rate > 0.5% or `block` hit rate spiking 10× — roll back immediately.

---

## 2. CEL context quick reference

Policy expressions can access the following variables, corresponding to `policy.DecideInput` (see SYSTEM-DESIGN §16.2).

```
envelope          client envelope (hint)
  .client.lp_version, .client.os
  .identity.user_id, .identity.team_id, .identity.machine_id
  .agent.tool, .agent.wire_protocol
  .session.session_id, .session.turn_index, .session.is_continuation
  .repo.repo_id, .repo.remote_url, .repo.branch, .repo.head_sha, .repo.is_dirty
  .context_signals.file_paths, .context_signals.primary_language
  .context_signals.diff_summary.lines_added, .lines_removed,
                                .contains_test_failure_keyword,
                                .contains_stack_trace
  .task_hints.task_type, .task_hints.complexity, .task_hints.data_sensitivity
  .task_hints.agentic_loop, .task_hints.contains_secret_like_pattern,
                            .task_hints.contains_pii_like_pattern,
                            .task_hints.security_sensitive_area
  .task_hints.confidence.<field>      # float [0,1]
  .scan.pre_scan_findings[].rule_id, .severity

server_class                          # server-authoritative fields
  .task_type, .complexity, .data_sensitivity
  .has_secret_finding, .findings[].type, .findings[].severity
  .pii_finding, .restricted_path_match
  .confidence.<field>

user                                  # auth resolved
  .id, .email, .role                 # role: developer|team_admin|platform_admin
team
  .id, .name, .tags
repo                                  # from repos.yaml
  .id, .owners, .sensitivity, .tags

budget
  .team_monthly_used_cents, .team_monthly_cap_cents
  .user_daily_used_cents, .user_daily_cap_cents
  .soft_hit, .hard_hit

# Built-in functions
rand()                                # [0,1); derived from trace_id, deterministic on replay
now()                                 # request received time, RFC3339
hour_of_day()                         # 0..23 (UTC)
day_of_week()                         # 1=Mon..7=Sun

# Constants (list/map) defined under the variables: section
restricted_repos, payments_repos, eu_only_models, ...
```

**Decision principles**

- **`server_class.*` is authoritative**; `envelope.task_hints.*` is only a hint. Security-related rules **must** prefer `server_class`.
- If you really must mix the two, gate with a `confidence > 0.x` threshold to filter client-side noise.
- `findings.exists(f, ...)` is a built-in CEL macro that iterates the list.
- String comparisons are case-sensitive; normalization is the responsibility of the server-side reclassifier.

---

## 3. Priority band conventions

Reserve gaps of 100 between bands so that newly added rules are not squeezed:

| Band | Purpose | Example ID prefix |
|---|---|---|
| 1000–1099 | Hard block (sensitive repo + secret) | P-SEC-BLOCK |
| 900–999 | Redact (secret / PII auto-placeholder) | P-SEC-REDACT |
| 800–899 | Budget hard cap / approval gate | P-BUDGET, P-APPROVE |
| 700–799 | Sensitivity upgrade route (private_strong) | P-SENS-UP |
| 500–699 | Main routing by task_type | P-ROUTE |
| 300–499 | Main routing by repo / team override | P-OVERRIDE |
| 100–299 | Experiments / shadow eval / log_only | P-EVAL, P-LOG |
| 0–99 | Fallthrough (usually via `defaults.on_no_match`) | — |

**Rule naming convention:** `P-<DOMAIN>-<NNN>`, where DOMAIN is an all-caps short word (SEC, ROUTE, BUDGET, APPROVE, SENS, OVERRIDE, EVAL, LOG).

---

## 4. Recipes — security / compliance

### 4.1 Restricted repo + any secret hit → hard block

```yaml
- id: P-SEC-BLOCK-001
  description: "Restricted repos: any secret-class finding -> hard block"
  priority: 1000
  when: |
    envelope.repo.repo_id in restricted_repos
    && server_class.has_secret_finding
  action: block
  reasons: ["restricted-repo", "secret-detected"]
```

### 4.2 Any provider key / private key / JWT / DB URL → redact placeholder

```yaml
- id: P-SEC-REDACT-001
  description: "Replace high-confidence credentials with placeholders"
  priority: 990
  when: |
    server_class.findings.exists(f,
      f.type in ["aws_key","gcp_key","azure_key","openai_key","anthropic_key",
                 "private_key","jwt","db_url","slack_token","github_token"]
      && f.severity == "critical")
  action: redact
  reasons: ["secret-redacted"]
```

### 4.3 PII (email / phone / ID number) → redact only on high-sensitivity repos

```yaml
- id: P-SEC-REDACT-002
  description: "PII redact only on high-sensitivity repos (avoid noise on normal code)"
  priority: 970
  when: |
    repo.sensitivity == "high"
    && server_class.pii_finding
  action: redact
  reasons: ["pii-on-high-sensitivity-repo"]
```

### 4.4 `security_review` task_type must use private_strong (no external boundary)

```yaml
- id: P-SENS-UP-001
  description: "Security reviews must stay on private models"
  priority: 750
  when: |
    server_class.task_type == "security_review"
  action: route_to_private_model
  model_pool: private_strong
  reasons: ["security-review-private-only"]
```

### 4.5 Restricted paths (e.g. `infra/secrets/`, `charts/values-prod.yaml`) → block

```yaml
- id: P-SEC-BLOCK-002
  description: "Restricted file paths block outright"
  priority: 1000
  when: |
    server_class.restricted_path_match
  action: block
  reasons: ["restricted-path"]

# Companion server-side rules in safety/restricted_paths.yaml:
# - "infra/secrets/**"
# - "charts/values-prod.yaml"
# - ".env*"
# - "**/*.pem"
```

### 4.6 Provider allowlist (compliance requires EU-resident providers only)

```yaml
- id: P-SENS-UP-002
  description: "EU-only repos must route to EU-resident providers"
  priority: 760
  when: |
    "eu-data-residency" in repo.tags
  action: route_to_private_model
  model_pool: eu_private_strong   # pools.yaml only attaches azure_eu / bedrock_eu

variables:
  eu_only_models:
    - "azure_openai:eu-gpt4o"
    - "anthropic_bedrock:eu-claude"
```

### 4.7 Client found a secret but the server did not reproduce → log_only (for rule-drift tuning)

```yaml
- id: P-LOG-001
  description: "Client-only secret signal -> log to observe FP/FN gap"
  priority: 200
  when: |
    envelope.task_hints.contains_secret_like_pattern
    && !server_class.has_secret_finding
  action: log_only
  reasons: ["client-server-scan-disagreement"]
```

---

## 5. Recipes — task-type routing

### 5.1 Cheap pool: summary / test_output / repo_search

```yaml
- id: P-ROUTE-001
  description: "Cheap pool for read-mostly tasks"
  priority: 500
  when: |
    server_class.task_type in ["summary","test_output","repo_search"]
    && server_class.data_sensitivity != "high"
  action: route
  model_pool: cheap
```

### 5.2 Standard pool: default code_edit / simple_edit / planning / file_reading (medium)

```yaml
- id: P-ROUTE-002
  description: "Standard pool for routine coding tasks"
  priority: 500
  when: |
    server_class.task_type in ["code_edit","simple_edit","planning","file_reading","review"]
  action: route
  model_pool: standard
```

### 5.3 Strong pool: architecture / debug

```yaml
- id: P-ROUTE-003
  description: "Strong pool for hard tasks"
  priority: 500
  when: |
    server_class.task_type in ["architecture","debug"]
  action: route
  model_pool: strong
```

### 5.4 file_reading + low sensitivity → downgrade to cheap

```yaml
- id: P-OVERRIDE-001
  description: "file_reading on low sensitivity repos -> cheap"
  priority: 550        # higher than P-ROUTE-002 to override it
  when: |
    server_class.task_type == "file_reading"
    && repo.sensitivity == "low"
  action: route
  model_pool: cheap
```

### 5.5 unknown task_type → standard, with logging

```yaml
- id: P-ROUTE-UNK-001
  description: "Unknown task -> standard, but log so we can train classifier"
  priority: 400
  when: |
    server_class.task_type == "unknown"
  action: route
  modifiers: [log_only]
  model_pool: standard
  reasons: ["unknown-task-type"]
```

### 5.6 Complexity high + primary route standard → escalate to strong

```yaml
- id: P-OVERRIDE-002
  description: "Bump high-complexity standard tasks up to strong"
  priority: 530
  when: |
    server_class.task_type in ["code_edit","planning","review"]
    && server_class.complexity == "high"
  action: escalate_to_strong_model
  reasons: ["complexity-bump"]
```

---

## 6. Recipes — data sensitivity / repo boundary

### 6.1 Restricted repo forced to private_strong

```yaml
- id: P-SENS-UP-003
  description: "Anything from restricted repos -> private_strong"
  priority: 720
  when: |
    envelope.repo.repo_id in restricted_repos
  action: route_to_private_model
  model_pool: private_strong

variables:
  restricted_repos:
    - "repo_payments_core"
    - "repo_keys_vault"
    - "repo_customer_pii_pipeline"
```

### 6.2 High-sensitivity repo (contains customer data) → private_strong + force raw redacted

```yaml
- id: P-SENS-UP-004
  description: "High sensitivity repos: private model + redacted-only raw"
  priority: 730
  when: |
    repo.sensitivity == "high"
  action: route_to_private_model
  modifiers: [redact]
  model_pool: private_strong
  reasons: ["high-sensitivity-repo"]
```

> Note: `storage.repo_overrides[<repo>].mode = redacted_only` is the config layer; this modifier also runs the in-flight redact step.

### 6.3 Team tag restriction (teams with `team.tags` including `pci-dss` cannot use third-party providers)

```yaml
- id: P-SENS-UP-005
  description: "PCI teams: only private_strong"
  priority: 740
  when: |
    "pci-dss" in team.tags
  action: route_to_private_model
  model_pool: private_strong
```

### 6.4 Sandbox repo (tag `sandbox`) allowed on cheap even if task_type normally requires higher

```yaml
- id: P-OVERRIDE-003
  description: "Sandbox repos: aggressive cost saving"
  priority: 560
  when: |
    "sandbox" in repo.tags
  action: route
  model_pool: cheap
```

---

## 7. Recipes — budget and approval

### 7.1 Team monthly soft cap (80%) → webhook warning (non-blocking)

> Note: the 80% soft warning fires from the `budget` service's built-in webhook; no policy needed. To enforce extra constraints at the policy layer, see the next recipe.

### 7.2 Team monthly hard cap → require_approval

```yaml
- id: P-BUDGET-001
  description: "Team monthly hard cap -> require approval"
  priority: 850
  when: |
    budget.hard_hit
  action: require_approval
  reasons: ["team-monthly-hard-cap"]
```

### 7.3 User daily cap hit → downgrade to cheap pool (non-blocking but limited)

```yaml
- id: P-BUDGET-002
  description: "User daily cap hit -> downgrade to cheap"
  priority: 820
  when: |
    budget.user_daily_used_cents >= budget.user_daily_cap_cents
  action: route
  model_pool: cheap
  reasons: ["user-daily-cap-hit"]
```

### 7.4 Large requests (diff > 2000 lines) + non-strong-required task → require_approval

```yaml
- id: P-APPROVE-001
  description: "Large diff requests gate to approval (catch runaway agentic loops)"
  priority: 830
  when: |
    envelope.context_signals.diff_summary.lines_added
      + envelope.context_signals.diff_summary.lines_removed > 2000
    && !(server_class.task_type in ["architecture","security_review"])
  action: require_approval
  reasons: ["oversized-diff"]
```

### 7.5 Agentic loop with too many turns (> 50 turns in the same session_id) → require_approval

> The server-side reclassifier injects `server_class.session_turn_count`.

```yaml
- id: P-APPROVE-002
  description: "Cap runaway agentic loops"
  priority: 840
  when: |
    server_class.session_turn_count > 50
  action: require_approval
  reasons: ["agentic-loop-runaway"]
```

---

## 8. Recipes — provider / model restrictions

### 8.1 OpenAI o-series reasoning models only allowed for architecture / debug (expensive)

> Implementation layer: `pools.yaml` only places `o3` in `strong`. To further restrict a specific team from using o3:

```yaml
- id: P-OVERRIDE-O3-001
  description: "Team 'frontend' cannot use o3 (cost-out)"
  priority: 580
  when: |
    team.id == "frontend"
    && server_class.task_type in ["architecture","debug"]
  action: route
  model_pool: standard           # do not let it reach strong
  reasons: ["frontend-no-o3"]
```

### 8.2 Force a provider offline (during an incident) → change `pools.yaml` weight=0; do not use policy

> Anti-pattern: using a policy to temporarily disable a provider pollutes rule history. Take-offlines and circuit-breakers belong in `pools.yaml` or the admin CLI: `aicg pools disable openai/gpt-4o`.

---

## 9. Recipes — shadow eval / experiments

### 9.1 Shadow-fork 5% of code_edit to strong (for downstream policy recommendation)

```yaml
- id: P-EVAL-001
  description: "Shadow eval 5% of standard code_edit on strong"
  priority: 100
  when: |
    server_class.task_type == "code_edit"
    && rand() < 0.05
  action: shadow_eval
  model_pool: strong               # shadow_pool
```

### 9.2 Shadow only during business hours / weekdays (cost control)

```yaml
- id: P-EVAL-002
  description: "Shadow eval only during business hours UTC"
  priority: 100
  when: |
    server_class.task_type == "planning"
    && day_of_week() <= 5
    && hour_of_day() >= 1 && hour_of_day() < 9
    && rand() < 0.10
  action: shadow_eval
  model_pool: strong
```

### 9.3 A/B: temporarily switch a team's standard primary routing to the strong model

```yaml
- id: P-EVAL-AB-001
  description: "A/B test: team 'platform' on strong for code_edit, 50%"
  priority: 540
  when: |
    team.id == "platform"
    && server_class.task_type == "code_edit"
    && rand() < 0.5
  action: route
  model_pool: strong
  reasons: ["AB-platform-code-edit-strong"]
```

---

## 10. Recipes — cross-provider feature protection

### 10.1 Requests using `cache_control` must not cross providers (avoid cache penalty)

> The routing engine will auto-strip and write `degraded_features`, but the cost may rise. Block this explicitly at the policy layer:

```yaml
- id: P-OVERRIDE-CACHE-001
  description: "Don't cross-provider when cache_control is in use"
  priority: 590
  when: |
    "cache_control" in envelope.task_hints.routing_hints
    && server_class.task_type in ["code_edit","planning"]
  action: route
  model_pool: standard           # standard pool internally prefers Anthropic
  reasons: ["preserve-cache-control"]
```

### 10.2 Requests with extended thinking must go to a thinking-capable strong pool

```yaml
- id: P-OVERRIDE-THINK-001
  description: "Extended thinking requests must stay on Anthropic-strong"
  priority: 600
  when: |
    "extended_thinking" in envelope.task_hints.routing_hints
  action: route
  model_pool: strong_thinking_only   # pools.yaml: attach only anthropic claude-opus
  reasons: ["preserve-extended-thinking"]
```

---

## 11. Putting it all together: a complete main.yaml [v0.2 canonical slots]

```yaml
# configs/policies/main.yaml
version: 1

defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["default-fallthrough"]

variables:
  restricted_repos:
    - "repo_payments_core"
    - "repo_keys_vault"
    - "repo_customer_pii_pipeline"

rules:
  # --- 1000-1099 hard block ---
  - id: P-SEC-BLOCK-001
    priority: 1000
    when: |
      envelope.repo.repo_id in restricted_repos
      && server_class.has_secret_finding
    action: block
    reasons: ["restricted-repo", "secret-detected"]

  - id: P-SEC-BLOCK-002
    priority: 1000
    when: server_class.restricted_path_match
    action: block
    reasons: ["restricted-path"]

  # --- 900-999 redact ---
  - id: P-SEC-REDACT-001
    priority: 990
    when: |
      server_class.findings.exists(f,
        f.type in ["aws_key","gcp_key","azure_key","openai_key","anthropic_key",
                   "private_key","jwt","db_url","slack_token","github_token"]
        && f.severity == "critical")
    modifiers: [redact]                       # [v0.2] canonical slot form
    reasons: ["secret-redacted"]

  - id: P-SEC-REDACT-002
    priority: 970
    when: repo.sensitivity == "high" && server_class.pii_finding
    modifiers: [redact]
    reasons: ["pii-on-high-sensitivity-repo"]

  # --- 800-899 budget / approval ---
  - id: P-APPROVE-001
    priority: 840
    when: server_class.session_turn_count > 50
    action: require_approval
    reasons: ["agentic-loop-runaway"]

  - id: P-APPROVE-002
    priority: 830
    when: |
      envelope.context_signals.diff_summary.lines_added
        + envelope.context_signals.diff_summary.lines_removed > 2000
      && !(server_class.task_type in ["architecture","security_review"])
    action: require_approval
    reasons: ["oversized-diff"]

  - id: P-BUDGET-001
    priority: 850
    when: budget.hard_hit
    action: require_approval
    reasons: ["team-monthly-hard-cap"]

  - id: P-BUDGET-002
    priority: 820
    when: budget.user_daily_used_cents >= budget.user_daily_cap_cents
    action: route
    model_pool: cheap
    reasons: ["user-daily-cap-hit"]

  # --- 700-799 sensitivity-up ---
  - id: P-SENS-UP-001
    priority: 750
    when: server_class.task_type == "security_review"
    action: route_to_private_model
    model_pool: private_strong
    reasons: ["security-review-private-only"]

  - id: P-SENS-UP-003
    priority: 730
    when: repo.sensitivity == "high"
    action: route_to_private_model               # PrimaryAction
    modifiers: [redact]                          # stacked
    model_pool: private_strong
    reasons: ["high-sensitivity-repo"]

  - id: P-SENS-UP-004
    priority: 720
    when: envelope.repo.repo_id in restricted_repos
    action: route_to_private_model
    model_pool: private_strong
    reasons: ["restricted-repo"]

  # --- 500-699 main routing ---
  - id: P-OVERRIDE-001
    priority: 560
    when: '"sandbox" in repo.tags'
    action: route
    model_pool: cheap
    reasons: ["sandbox-cost-saving"]

  - id: P-OVERRIDE-002
    priority: 550
    when: |
      server_class.task_type == "file_reading"
      && repo.sensitivity == "low"
    action: route
    model_pool: cheap

  - id: P-OVERRIDE-003
    priority: 530
    when: |
      server_class.task_type in ["code_edit","planning","review"]
      && server_class.complexity == "high"
    modifiers: [escalate_to_strong_model]        # [v0.2] modifier slot
    reasons: ["complexity-bump"]

  - id: P-ROUTE-001
    priority: 500
    when: |
      server_class.task_type in ["summary","test_output","repo_search"]
      && server_class.data_sensitivity != "high"
    action: route
    model_pool: cheap

  - id: P-ROUTE-002
    priority: 500
    when: |
      server_class.task_type in ["code_edit","simple_edit","planning",
                                 "file_reading","review"]
    action: route
    model_pool: standard

  - id: P-ROUTE-003
    priority: 500
    when: |
      server_class.task_type in ["architecture","debug"]
    action: route
    model_pool: strong

  - id: P-ROUTE-UNK-001
    priority: 400
    when: server_class.task_type == "unknown"
    action: route
    side_effects: [log_only]                     # [v0.2] log_only is side_effect, not modifier
    model_pool: standard
    reasons: ["unknown-task-type"]

  # --- 100-299 experiments / log ---
  - id: P-LOG-001
    priority: 200
    when: |
      envelope.task_hints.contains_secret_like_pattern
      && !server_class.has_secret_finding
    side_effects: [log_only]                     # [v0.2] side_effect slot
    reasons: ["client-server-scan-disagreement"]

  - id: P-EVAL-001
    priority: 100
    when: |
      server_class.task_type == "code_edit"
      && rand() < 0.05
    side_effects: [shadow_eval]                  # [v0.2]
    shadow_pool: strong                          # [v0.2] explicit shadow_pool field
```

---

## 12. Testing and rollout

### 12.1 Local validation

```bash
aicg policyctl validate configs/policies/main.yaml
# - YAML schema check
# - CEL compile check (every `when` must compile)
# - rule_id uniqueness
# - priority-band lint (ID falls into the agreed band)
# - reasons non-empty (required for block / redact / require_approval)
# - default.on_no_match exists
```

### 12.2 Decision simulation

```bash
# Replay against a recorded envelope fixture; inspect the expected Decision
aicg policyctl simulate \
  --policy configs/policies/main.yaml \
  --fixture tests/fixtures/envelope_001_payments_secret.json
```

Or use the dashboard `GET /api/v1/policy/decision/explain` (see SYSTEM-DESIGN §13.6).

### 12.3 Canary

- Do not flip the entire fleet at once. Run the dashboard `policy preview` mode for 10 minutes (use `log_only` shadow evaluation without changing the real action).
- Watch the `decision_diff` report (old policy vs. new policy on the same set of traces).
- Once satisfied, `aicg policyctl reload`; inotify triggers an atomic switch in GW.

### 12.4 Rollback

- `configs/policies/` is git-tracked; rollback = `git revert` + reload.
- The GW keeps the last compiled artifact in memory; if reload fails, the previous version is preserved (see SYSTEM-DESIGN §16.2 `Reload`).

---

## 13. Anti-patterns and pitfalls

| Anti-pattern | Why not | Use instead |
|---|---|---|
| Using `envelope.task_hints.task_type` for security decisions | Client hints are untrusted and bypassable | `server_class.task_type` |
| Encoding "provider temporarily offline" as a policy `block` | Conflates policy with infrastructure responsibilities; pollutes rule history | Adjust weights in `pools.yaml`; admin CLI for circuit-breaks |
| Multiple terminal-allow rules with the same priority | Match order is undefined; decisions are not reproducible | Spread priority bands; keep each rule's priority unique |
| `when` with more than 5 chained `&&` / `||` | Hard to debug; explain output is unreadable | Split into multiple rules; use `variables:` for sets |
| Free-form text in `reasons` | The dashboard cannot aggregate by reason | Use a controlled vocabulary (`secret-detected`, `restricted-repo`, `high-sensitivity-repo`, ...) |
| Adding `modifiers: [log_only]` to every rule | Log spam; audit table explodes | Only during canary for new rules; remove once stable |
| Using `rand()` as a security toggle | Result varies each time; incident not reproducible | rand only for experiments; security uses deterministic conditions |
| `priority` numbers crammed together (500/501/502) | No room to insert new rules later | Use band gaps ≥ 10 |
| Setting `model_pool` on an action other than `route` | The engine ignores it, but maintainers misread it | Do not set `model_pool` on block / require_approval / redact |
| Writing PII detection as a subclause of `task_type == "code_edit"` | Misses non-code_edit traffic | Use a separate P-SEC-REDACT rule with its own priority |
| One rule containing both `route` and `redact` without modifiers | Diverges from conflict-resolver semantics | `action: route, modifiers: [redact]` (or two rules, letting the stacking mechanism merge them) |

---

## 14. Checklist: 14 must-check items before going live [v0.2.3]

- [ ] All `when` clauses pass `aicg policyctl validate`
- [ ] Every `block` / `redact` / `require_approval` has a non-empty `reasons`
- [ ] Three-slot verbs used correctly (`action:` is PrimaryAction only; `modifiers:` / `side_effects:` each from its own column)
- [ ] `defaults.on_no_match` is set and the pool exists in `pools.yaml`
- [ ] All `model_pool` / `shadow_pool` references can be resolved in `pools.yaml`
- [ ] **Every pool member's `endpoint_id` is registered in `pools.yaml::provider_endpoints`; any change to the registry has codeowners' dual approval (CODEOWNERS lists `configs/pools.yaml`)** [v0.2.2]
- [ ] Variables like `restricted_repos` are aligned with `repos.yaml` (no dangling IDs)
- [ ] Server-side reclassifier rules have been deployed for new fields (e.g. `restricted_path_match`)
- [ ] No client hint is used for security decisions (unless an explicit `confidence > 0.x` is applied)
- [ ] No priority collisions across bands
- [ ] CI runs fixture replays with `decision diff` within expectation (including `seed` consistency check) [v0.2]
- [ ] Dashboard `policy preview` has run for ≥ 10 minutes
- [ ] Webhook (alert / approval) configured and tested
- [ ] Rollback plan: the previous version's git tag is recorded

---

**End of cookbook v0.2.2.**
