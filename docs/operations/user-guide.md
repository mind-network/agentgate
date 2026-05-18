# AgentGate User Guide

> Version: v0.2.3 | Date: 2026-05-10
> Audience: developers using AgentGate
> Companion docs: SYSTEM-DESIGN.md, dogfood-runbook.md

## 1. Introduction

AgentGate is an enterprise-grade AI Coding gateway. You run **Local Proxy (LP)** on your machine; it forwards API requests from tools like Claude Code, Cursor, and Aider to the enterprise gateway, which handles policy checks, route selection, cost attribution, and security scanning.

For developers, AgentGate is largely transparent in daily use — install the LP, log in once, set your environment variables, and use the tools as if you were calling the API directly.

### 1.1 Core concepts

| Concept | Meaning |
|---|---|
| LP (Local Proxy) | Local daemon (`aicg`) running on your machine |
| GW (Gateway) | Enterprise-side gateway that governs all traffic |
| API Key | Your long-lived access credential, obtained via `aicg login` |
| Repo Binding | Repository binding token the GW uses to identify your code repo |
| Session | Conversational context within a single LP run |

## 2. Installation

### 2.1 macOS

```bash
brew install agentgate/aicg/aicg
```

### 2.2 Linux

```bash
npm install -g @agentgate/aicg
# or download a binary from GitHub Releases
```

### 2.3 Verify installation

```bash
aicg version
# aicg 0.3.1 (0e7a2b1) darwin/arm64
```

## 3. First-time setup

> If you are a day-one dogfood team member, read [`dogfood-onboarding.md`](./dogfood-onboarding.md) §4 (D-Day developer) first — it covers the invite / setup-token login paths plus a one-shot curl verification. This section covers steady-state daily use.

### 3.1 Obtain an invite link

Contact your platform_admin / AI Platform team for an invite token. The admin will give you a command similar to:

```bash
aicg login --invite <invite-token> --gateway <gw-url>
```

The current LP CLI parser only accepts space-separated flag values; do not use the `--invite=<token>` / `--gateway=<url>` form when copy-pasting commands.

### 3.2 Run the login

```bash
aicg login --invite aicg_invite_xxxxxxxxxxxxxxxx \
           --gateway https://gw.example.com:8443 \
           --ca /path/to/ca.crt
```

On successful login:
- `~/.aicg/credentials` stores your API key (64 hex characters)
- `~/.aicg/config.yaml` stores the gateway URL and related settings

Verify:

```bash
cat ~/.aicg/credentials
# gateway_url: https://gw.example.com:8443
# user_id: alice
# api_key: <64-char-hex>
```

### 3.3 Set environment variables

`aicg env` prints a shell snippet — append it to your rc file:

```bash
aicg env >> ~/.zshrc
source ~/.zshrc
```

Output:

```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:7777/anthropic"
export ANTHROPIC_API_KEY="sk-aicg-noop"
export OPENAI_BASE_URL="http://127.0.0.1:7777/openai/v1"
export OPENAI_API_KEY="sk-aicg-noop"
```

> `API_KEY` is set to a placeholder; the LP swaps in your real API key after receiving the request.
>
> ⚠️ `ANTHROPIC_BASE_URL` must end with `/anthropic`, with no trailing slash. Claude Code appends `/v1/messages`, so the full request path is `http://127.0.0.1:7777/anthropic/v1/messages`. Missing `/anthropic` returns a 404.

## 4. Daily use

### 4.1 Start the LP

```bash
aicg start --port 7777
```

LP runs in the background, listening on `127.0.0.1:7777`.

Foreground (for debugging):

```bash
aicg start --foreground
```

### 4.2 Check status

```bash
aicg status
# LP: running (pid=12345, port=7777)
# Gateway: https://gw.example.com:8443  connected
# Session: abc123...  turns=3
# Cost today: $0.42
# Cost this month: $12.80 / $500.00
```

### 4.3 Stop the LP

```bash
aicg stop
```

### 4.4 Self-check

```bash
aicg doctor
```

Checks gateway connectivity, credential validity, repo-binding status, and environment-variable configuration.

## 5. AI tool integration

AgentGate is transparent to tools — once the relevant `BASE_URL` and `API_KEY` environment variables are set, the tools need no further configuration.

### 5.1 Claude Code

Claude Code reads `ANTHROPIC_BASE_URL` by default, so it routes through LP automatically once the env vars are set:

```bash
# Confirm the env var
echo $ANTHROPIC_BASE_URL
# http://127.0.0.1:7777/anthropic

# Use Claude Code normally
claude
```

### 5.2 Cursor / Aider / Continue

These tools use the OpenAI-compatible API. Setting `OPENAI_BASE_URL` is enough:

```bash
# Cursor: Settings → Models → OpenAI Base URL = http://127.0.0.1:7777/openai/v1
# Aider:
export OPENAI_API_BASE=http://127.0.0.1:7777/openai/v1
aider --model gpt-4o
```

### 5.3 Direct curl debugging

```bash
# Anthropic wire
curl -s http://127.0.0.1:7777/anthropic/v1/messages \
  -H "Content-Type: application/json" \
  -H "x-api-key: lp-noop" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4-6","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}'

# OpenAI wire
curl -s http://127.0.0.1:7777/openai/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer lp-noop" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"hello"}]}'
```

### 5.4 Inspect aicg.* events (dev only)

```bash
aicg start --foreground --passthrough-aicg-events
# then:
curl -N http://127.0.0.1:7777/anthropic/v1/messages -d @req.json | grep '^event: aicg'
```

> `--passthrough-aicg-events` is forbidden in production; the LP refuses to start with this flag set.

## 6. Repo binding

### 6.1 Automatic binding

The first time you make a request from a repository, the LP completes binding automatically:

1. LP computes `repo_id = sha256(remote_url)`
2. Requests a binding token from the GW
3. Writes `.git/aicg-binding`

### 6.2 Manual binding

```bash
aicg bind-repo --path /path/to/repo
```

### 6.3 Show current binding

```bash
aicg bind-repo --show
# repo_id: repo_acme_payments
# bound_at: 2026-05-10T09:30:00Z
# status: active
```

### 6.4 Common issues

| Symptom | Cause | Fix |
|---|---|---|
| Request returns 403 | Repo not bound or binding expired | Rerun `aicg bind-repo` |
| `aicg doctor` reports binding failed | Repo has no remote URL | `git remote add origin <url>` |
| Migrated to a new machine | Old binding token no longer exists | Rerun `aicg bind-repo` |

## 7. Policies and permissions

### 7.1 Show applicable policies

```bash
aicg policy show --repo . --task code_edit
```

Prints a summary of policies that would match for the current repo and task type (read-only; shows only policies that apply to you).

### 7.2 Blocked requests

When a policy decides `block`, the LP returns 451 with a reason payload:

```json
{
  "error": {
    "type": "aicg_blocked",
    "message": "request blocked by policy",
    "policy_reason": "restricted-repo + secret-detected",
    "rule_id": "P-SEC-001"
  }
}
```

Contact your team_admin or platform_admin for details.

### 7.3 Approval required

When a policy decides `require_approval`, the LP returns 202 with `X-AICG-Approval-Id`:

```bash
# The LP prompt looks like:
# Request requires approval. Approval ID: appr_xxxxxxxx
# Waiting for admin approval...
# Check status: aicg approval status appr_xxxxxxxx
```

## 8. Cost visibility

### 8.1 CLI

```bash
# Current-month total (default)
aicg stats
aicg stats --this-month
# Today's breakdown
aicg stats --today
# Group by dimension
aicg stats --by team
aicg stats --by user
aicg stats --by repo
aicg stats --by model
# Dimension aliases
aicg stats --by-team
aicg stats --by-user
aicg stats --by-repo
aicg stats --by-model
# Combined
aicg stats --today --by-model
```

Sample output (default `--this-month --by team`):

```
Cost Summary  (2026-05-01 – 2026-06-01  ·  by team)
  dim_value            requests   tok_in  tok_out   failed      cost
  engineering                 42    15000     8000        2     12.50
  platform                    18     5000     3000        0      5.00
```

### 8.2 Dashboard

Visit the GW's `/dashboard` page (browser login required) for detailed cost views and historical trends.

## 9. Configuration reference

### 9.1 Configuration files

| File | Contents |
|---|---|
| `~/.aicg/config.yaml` | Gateway URL, port, TLS settings |
| `~/.aicg/credentials` | user_id, API key (mode 0600) |
| `.git/aicg-binding` | Repo binding token (managed automatically by LP) |

### 9.2 config.yaml settings

```yaml
gateway_url: "https://gw.example.com:8443"
port: 7777
tls_ca_cert: "/path/to/ca.crt"
log_level: "info"      # debug | info | warn | error
```

Restart the LP after changes:

```bash
aicg config set gateway_url=https://gw2.example.com:8443
aicg stop && aicg start
```

### 9.3 Full CLI reference

```
aicg login    [--invite <token>] [--gateway <url>] [--ca <path>]  Log in and obtain an API key
aicg logout                                                        Clear local credentials
aicg start    [--port 7777] [--config <path>] [--foreground]       Start the LP
aicg stop                                                          Stop the LP
aicg status                                                        Runtime status
aicg bind-repo [--path .] [--show]                                 Repo binding
aicg config   {show, set <key>=<value>}                            Manage configuration
aicg policy   show [--repo .] [--task <type>]                      Show applicable policies
aicg env                                                            Print env-var snippet
aicg version                                                       Version info
aicg doctor                                                        Self-check diagnostic
aicg stats    [--today|--this-month|--by-repo|--by-model]          Cost statistics
aicg approval status <approval_id>                                 Approval status query
```

## 10. Troubleshooting

### 10.1 LP fails to start

| Symptom | Likely cause | Fix |
|---|---|---|
| `port already in use` | Port 7777 already taken | `aicg start --port 7778` |
| `gateway unreachable` | Gateway unreachable or VPN down | `aicg doctor` to verify connectivity |
| `credentials not found` | Not logged in or credentials corrupted | `aicg login` again |
| `TLS certificate error` | CA cert path wrong or expired | Confirm `tls_ca_cert` is correct |

### 10.2 Request failures

| Error | Meaning | Action |
|---|---|---|
| 404 (Claude Code) | `ANTHROPIC_BASE_URL` is missing the `/anthropic` suffix | Set it to `http://127.0.0.1:7777/anthropic` with no trailing slash |
| 401 | Auth failure (API key expired) | `aicg logout && aicg login` |
| 403 | Repo binding failed | `aicg bind-repo` |
| 451 | Blocked by policy | Inspect `policy_reason`, contact your admin |
| 429 | Rate limited | Wait and retry |
| 502/503 | GW or upstream provider failure | Wait briefly and retry; if persistent, contact admin |
| `Connection refused` | LP not running | `aicg start` |

### 10.3 LP running but tools still fail

```bash
# 1. Confirm LP is running
aicg status

# 2. Confirm env vars point to the right port
echo $ANTHROPIC_BASE_URL

# 3. Confirm LP can reach the GW
aicg doctor

# 4. Direct curl test
curl http://127.0.0.1:7777/_aicg/health
```

### 10.4 Collect diagnostics

```bash
aicg doctor 2>&1 | tee aicg-doctor-$(date +%Y%m%d%H%M).log
```

Attach this log when reaching out to an admin.

## 11. Direct-connect fallback (admin-controlled)

When the GW is unreachable, the LP defaults to fail-closed (request rejected with an error). Admins can enable direct-connect fallback:

```yaml
# ~/.aicg/config.yaml (admin-distributed)
fallback:
  on_gw_unreachable: direct_cheap   # default: off
  direct_endpoint: anthropic-prod   # which provider to fall back to
```

> Direct-connect fallback only takes effect when the GW is fully unreachable, and traffic can only be routed to the cheap pool (limited models, limited features). This setting is gated by admin decision; ordinary users cannot enable it themselves.

## 12. Uninstall

```bash
aicg logout          # Clear credentials
aicg stop            # Stop the LP
# macOS
brew uninstall aicg
# Clean up local files
rm -rf ~/.aicg
```
