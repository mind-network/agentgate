# Config Reload & Secret Rotation — Operator Runbook

## Overview

AgentGate GW hot-reloads all runtime configuration and file:// secrets via fsnotify (inotify).
No image rebuild or GW restart is required for config/secret changes on Linux hosts.
On Docker Desktop (macOS/Windows), fsnotify across bind mounts may be unreliable;
`docker compose restart gw` is the supported fallback.

**Rebuild is never required for config or secret changes.**

## Files Under Hot Reload

All six YAML files are watched and reloaded atomically within ≤5s of a host-side edit.
At P0, four files also have live request-path effects:

| File | Container Path | Env Override |
|---|---|---|
| `pools.yaml` | `/configs/pools.yaml` | `AICG_CONFIG_POOLS` |
| `pricing.yaml` | `/configs/pricing.yaml` | `AICG_CONFIG_PRICING` |
| `policies/main.yaml` | `/configs/policies/main.yaml` | `AICG_CONFIG_POLICY` |
| `identity/teams.yaml` | `/configs/identity/teams.yaml` | `AICG_CONFIG_TEAMS` |

Two identity files are reload-only at P0: the GW accepts and validates edits without
restart, but they do not yet change request-path behavior.

| File | Container Path | Env Override |
|---|---|---|
| `identity/users.yaml` | `/configs/identity/users.yaml` | `AICG_CONFIG_USERS` |
| `identity/repos.yaml` | `/configs/identity/repos.yaml` | `AICG_CONFIG_REPOS` |

file:// secrets under `/run/agentgate/secrets/` (bind-mounted from `./configs/secrets/`) are
also watched and re-resolved on change.

P0 policy validation accepts only `action: route` and `action: allow`. Writing
`action: block`, `deny`, `redact`, or `require_approval` fails validation; the
previous policy snapshot remains active and `config_reload_total{file="main.yaml",result="validate_error"}`
increments. Those actions are tracked under SYSTEM-DESIGN §20 P1+ readiness.

### Config Path Precedence

The GW resolves each config file path with three-tier precedence:

1. **Per-file env var** (e.g. `AICG_CONFIG_POOLS=/custom/pools.yaml`) — highest priority
2. **`AICG_CONFIG_DIR` umbrella** — e.g. `AICG_CONFIG_DIR=/opt/configs` → `/opt/configs/pools.yaml`
3. **Hardcoded default** — `/etc/agentgate/<file>` for bare-binary deploys

Docker Compose already sets all six per-file vars to `/configs/...`, so the umbrella env is
only needed for non-Compose deploys.

## Editing Flow

### Via Hot Reload (Linux hosts, Docker Compose)

```sh
# Edit any config file on the host.
vim ./configs/pricing.yaml

# The GW reloads within 5s. Verify:
curl -sk https://localhost:8443/metrics | grep config_reload_total
curl -sk https://localhost:8443/healthz
```

### Via SIGHUP (manual reload)

When automated fsnotify is unavailable (e.g. distroless containers with no shell),
send SIGHUP to trigger a reload through the same code path:

```sh
kill -HUP <gw-pid>
# Or from outside the container:
docker compose kill -s HUP gw
```

The reload emits `config_reload_total{file="<sighup>",result="success"}` and logs
"SIGHUP received, triggering manual config reload".

### Via Restart (Docker Desktop fallback)

```sh
vim ./configs/pricing.yaml
docker compose restart gw
```

No `docker compose build` is required.

## Verifying a Reload Took Effect

### Check the Prometheus counter

```sh
curl -sk https://localhost:8443/metrics | grep config_reload_total
```

Successful reloads increment `config_reload_total{file="<basename>",result="success"}` where
`<basename>` is the filename that triggered the reload (e.g. `pricing.yaml`, `pools.yaml`).
Parse/validate failures increment `config_reload_total{file="<basename>",result="parse_error"}`.

### Check structured logs

```sh
docker compose logs gw | grep config_reload
```

Success:
```
event=config_reload result=success duration_ms=12
```

Failure (last-known-good retained):
```
event=config_reload file=pricing.yaml result=parse_error error="yaml: line 5: ..." duration_ms=3
```

## Last-Known-Good Guarantee

If a config file edit introduces a parse or validation error, the GW:

1. Logs a structured warning with `result=parse_error`
2. Increments `config_reload_total{result="parse_error"}`
3. **Retains the previous valid in-memory config**
4. Continues serving requests with the old config

**No request is ever served with a partially-loaded or invalid config.**

To recover: fix the YAML on the host and the next fsnotify event triggers a successful reload.

## file:// Secret Rotation

### Setup

1. Place the secret file in `./configs/secrets/`:
   ```sh
   echo -n "sk-ant-my-key" > ./configs/secrets/anthropic-prod
   ```

2. Reference it in `pools.yaml`:
   ```yaml
   provider_endpoints:
     anthropic-prod:
       wire: anthropic
       vendor: anthropic
       url: "https://api.anthropic.com"
       key_ref: "file:///run/agentgate/secrets/anthropic-prod"
   ```

3. The container sees the file at `/run/agentgate/secrets/anthropic-prod` via the bind mount.

### Rotation Procedure

```sh
# Write the new key to the secret file.
echo -n "sk-ant-new-key" > ./configs/secrets/anthropic-prod

# The GW detects the file change, re-resolves the key, and atomically swaps
# the in-memory snapshot. The new key is effective for the next outbound request.
# No restart needed.
```

### Verification

Check the Prometheus counter:
```sh
curl -sk https://localhost:8443/metrics | grep secret_reload_total
```

Successful rotations increment `secret_reload_total{ref="file",result="success"}`.
Failed secret resolutions increment `secret_reload_total{ref="file",result="resolve_error"}`.

### Security

- The `configs/secrets/` directory is gitignored. **Do not force-add files under it.**
- Plaintext keys **never appear in logs**. The structured log for secret reloads contains
  only the scheme (`file`) and result, never the resolved value.
- Audit events record the `key_ref` string only, never the plaintext key (SYSTEM-DESIGN §10.x).

## Bare-Binary (Non-Docker) Deploy

When running `aicg-gw` as a bare binary:

```sh
# Place configs in /etc/agentgate/ (the default path).
sudo mkdir -p /etc/agentgate/policies /etc/agentgate/identity
sudo cp pools.yaml pricing.yaml /etc/agentgate/
sudo cp policies/main.yaml /etc/agentgate/policies/
sudo cp identity/*.yaml /etc/agentgate/identity/

# Or set AICG_CONFIG_DIR:
export AICG_CONFIG_DIR=/opt/agentgate/configs
aicg-gw
```

If no config volume is mounted and no env vars point to config files, the GW fails fast:
```
config load failed: read pools.yaml: open /etc/agentgate/pools.yaml: no such file or directory
```

This proves configs are not baked into the binary.

## Metrics Reference

| Counter | Labels | Meaning |
|---|---|---|
| `config_reload_total` | `file`, `result` | Config reload attempts. `file` is the basename of the changed file (e.g. `pricing.yaml`) or `<sighup>` for manual SIGHUP-triggered reloads; `result` ∈ {success, parse_error, validate_error} |
| `secret_reload_total` | `ref`, `result` | Secret reload attempts. `ref` is the scheme (`file`); `result` ∈ {success, resolve_error} |

Both counters are exposed at `GET /metrics` (Prometheus text format).

## Troubleshooting

### Edit didn't take effect

1. Check the counter: `curl -sk https://localhost:8443/metrics | grep config_reload_total`
2. Check GW logs: `docker compose logs gw | grep config_reload`
3. On Docker Desktop, fsnotify may drop events. Use `docker compose restart gw` as fallback.

### GW is down after config edit

The last-known-good guarantee should prevent this. If the GW crashes:
1. Check `docker compose logs gw` for the error.
2. Fix the YAML syntax on the host.
3. `docker compose restart gw`

### Secret not rotating

1. Verify the file path in `key_ref` matches the bind mount: `file:///run/agentgate/secrets/<name>`
2. Check `docker compose exec gw ls /run/agentgate/secrets/` to confirm the file is visible.
3. Check `secret_reload_total` counter.

## References

- SYSTEM-DESIGN §10.x — Secret Resolver semantics
- SYSTEM-DESIGN §15 — Config layout
- `docker-compose.yaml` — Bind mount and env var configuration
