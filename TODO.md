# TODO

## Handoff Backlog

Deferred handoff candidates surfaced during planning. Numbers are not reserved here;
the writer assigns the next `HANDOFF-NNN` when materializing one of these.

### Admin reload HTTP endpoint: POST /admin/reload
- **Objective**: Expose `POST /admin/reload` (or `POST /api/v1/admin/reload`) that triggers `HotReloader.TriggerReload()` over HTTP, so dashboard / Ops tooling can reload config without OS-signal access.
- **Parent / expected dependency**: HANDOFF-010 (introduces `TriggerReload()` and the SIGHUP entry).
- **Scope summary**: New chi route mounted under an admin path, guarded by the existing `auth.RequireRole(auth.RolePlatformAdmin)` middleware (`internal/gw/auth/auth.go:23-24`), audited via `audit.Writer` (new event type), idempotent semantics (always 200 with current Overall reload result). Touches `cmd/aicg-gw/main.go` route wiring + a thin handler in `internal/gw/edge/` or a new `internal/gw/admin/` package.
- **Reason deferred**: Adds an externally-reachable mutating endpoint with auth + audit obligations. SIGHUP from HANDOFF-010 already covers the "manual reload" need at lower surface cost; do not bundle.

### GW debug image variant for ops dev environments
- **Objective**: Provide an alternative container image (e.g. `aicg-gw:dev-debug` based on `gcr.io/distroless/base-debian12:debug` or a thin `alpine`) that includes a shell + coreutils, so ops engineers can `kubectl exec` / `docker exec` into a running GW for diagnostics in non-production environments.
- **Parent / expected dependency**: None (independent ops/build concern).
- **Scope summary**: Add a second Dockerfile target (`Dockerfile.debug` or a multi-stage `--target debug`) building from a debug-friendly base; wire `make build-debug-image` and document the dev-only nature in `docs/operations/`. Production tag must keep `gcr.io/distroless/static-debian12` for the security boundary.
- **Reason deferred**: Pure ops/dev ergonomics, no GW behavior change. Belongs in its own ops-thread handoff.

### LP gwclient mTLS client cert support
- **Objective**: Let `internal/lp/gwclient.NewClient` optionally load a client certificate + key so the LP can authenticate to a GW that requires mutual TLS, in addition to today's server-CA-only verification.
- **Parent / expected dependency**: None (independent transport-security concern). Touches the same surface as a future GW-side `tls.Config.ClientAuth = RequireAndVerifyClientCert` rollout but does not depend on it.
- **Scope summary**: `internal/gw/server/config_setup.go` (or wherever LP `gwclient.Config` is wired) gains optional `client_cert_path` + `client_key_path`; `internal/lp/gwclient/client.go:41-54` loads them via `tls.LoadX509KeyPair` and sets `transport.TLSClientConfig.Certificates`. CLI `aicg-lp doctor` should surface the cert chain identity. Tests under `internal/lp/gwclient/` extend the existing TLS setup helpers.
- **Reason deferred**: P0 only mandates "mTLS optional" and the GW side has not enabled `ClientAuth`, so the LP side has no peer to authenticate against. User-visible value is zero today; ergonomic + security value materializes when paired with the GW-side switch. Defer until the GW rollout schedules it; do not bundle with retry budget below — they live in different reliability vs. authentication slices.

### LP gwclient retry budget parameterization
- **Objective**: Replace the hardcoded "retry once on pre-first-byte 5xx" in `internal/lp/gwclient.doWithRetry` with a configurable retry budget so ops can tune attempt count, optional backoff/jitter, and (eventually) cross-request budget pools without code changes.
- **Parent / expected dependency**: None. Must continue to honor §5.4.4 first-byte state machine — any budget value still produces zero retries once `FirstByteStateMachine.FlushFirstByte()` has fired.
- **Scope summary**: `internal/lp/gwclient/client.go:78-117` currently writes `attempt <= 1` implicitly: the function does one initial call + one retry on 5xx (lines 94-99 then 103-114), with no backoff and no attempt cap from config. Add a `RetryBudget` struct (`MaxAttempts int`, optional `InitialBackoff time.Duration`, `MaxBackoff time.Duration`, jitter knob), thread it via `Client` or `Config`, and rewrite `doWithRetry` as a bounded loop. Tests in `internal/lp/gwclient/client_test.go` cover attempts=0 (pass-through), attempts=1 (current behavior), attempts>1 with backoff, and "no retry after first-byte" invariant.
- **Reason deferred**: §5.4.4 only requires "retry once" and the current implementation already meets the spec letter for letter. Audit YELLOW is for "treat this as a configurable resource", not a behavior gap. P0 dogfood traffic has not surfaced any operational need to tune the attempt count. Pick up once we have either (a) production retry-rate telemetry that argues for it, or (b) an ops request to disable retry entirely (e.g. for idempotency-sensitive endpoints).
