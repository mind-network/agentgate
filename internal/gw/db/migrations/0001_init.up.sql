-- 0001_init.up.sql — P0 MVP schema (8 tables)
-- See SYSTEM-DESIGN v0.2.3: §11.3 (raw_record), §11.5 (raw_access_audit),
-- §12.1 (audit_event), §12.2 (cost_event), §12.3 (routing_event),
-- §12.4 (approval_request), §12.5 (budget_reservation), audit_chain_root.

-- 1. raw_record — prompt/response storage metadata
CREATE TABLE raw_record (
    trace_id        UUID PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    team_id         TEXT NOT NULL,
    repo_id         TEXT,
    sensitivity     TEXT CHECK (sensitivity IN ('low','medium','high','unknown')),
    storage_policy  TEXT NOT NULL CHECK (storage_policy IN
                       ('metadata_only','redacted_only','full','disabled')),
    -- P2+: only filled when storage_policy ∈ {redacted_only, full}
    object_uri      TEXT,
    object_size     INTEGER,
    kek_id          TEXT,
    redacted_fields JSONB,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ NOT NULL,
    deleted_at      TIMESTAMPTZ,

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

-- 2. audit_event — append-only audit log
-- P0/P1: self_hash and prev_hash are NULL; P3+ hash chain enabled.
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
    decision        JSONB,
    rule_ids        TEXT[],
    detail          JSONB,
    request_summary JSONB,
    routed_to       TEXT,
    fallback_chain  JSONB,
    error_code      TEXT,
    prev_hash       BYTEA,
    self_hash       BYTEA
);
CREATE INDEX ON audit_event (trace_id);
CREATE INDEX ON audit_event (tenant_id, team_id, event_at DESC);
CREATE INDEX ON audit_event (tenant_id, repo_id, event_at DESC);

-- 3. cost_event — per-request cost accounting
CREATE TABLE cost_event (
    id              BIGSERIAL PRIMARY KEY,
    event_at        TIMESTAMPTZ NOT NULL,
    trace_id        UUID NOT NULL,
    attempt_no      SMALLINT NOT NULL,
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    team_id         TEXT NOT NULL,
    repo_id         TEXT,
    task_type       TEXT,
    policy_rule_id  TEXT,
    provider        TEXT NOT NULL,
    endpoint_id     TEXT NOT NULL,
    model           TEXT NOT NULL,
    pool            TEXT NOT NULL,
    is_private      BOOLEAN NOT NULL,
    input_tokens    INTEGER NOT NULL,
    output_tokens   INTEGER NOT NULL,
    cache_read_tokens     INTEGER DEFAULT 0,
    cache_create_tokens   INTEGER DEFAULT 0,
    cost_cents      INTEGER NOT NULL,
    cost_source     TEXT NOT NULL,
    latency_ms      INTEGER,
    success         BOOLEAN NOT NULL,
    error_class     TEXT
);
CREATE INDEX ON cost_event (event_at DESC);
CREATE INDEX ON cost_event (tenant_id, team_id, event_at DESC);
CREATE INDEX ON cost_event (tenant_id, user_id, event_at DESC);
CREATE INDEX ON cost_event (tenant_id, repo_id, event_at DESC);
CREATE INDEX ON cost_event (provider, model, event_at DESC);

-- 4. routing_event — routing decision audit
CREATE TABLE routing_event (
    event_at        TIMESTAMPTZ NOT NULL,
    trace_id        UUID NOT NULL,
    attempt_no      SMALLINT NOT NULL,
    tenant_id       TEXT NOT NULL,
    decision        JSONB NOT NULL,
    pool_selected   TEXT,
    member_selected JSONB,
    fallback_chain  JSONB,
    degraded_features TEXT[],
    breaker_state   TEXT,
    seed            BYTEA,
    PRIMARY KEY (trace_id, attempt_no)
);
CREATE INDEX ON routing_event (event_at DESC);
CREATE INDEX ON routing_event (tenant_id, event_at DESC);

-- 5. approval_request — P2+ approval workflow (P0 schema-only, inert)
CREATE TABLE approval_request (
    id              UUID PRIMARY KEY,
    trace_id        UUID NOT NULL,
    tenant_id       TEXT NOT NULL,
    requestor       TEXT NOT NULL,
    team_id         TEXT NOT NULL,
    repo_id         TEXT,
    decision_snapshot JSONB NOT NULL,
    state           TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at     TIMESTAMPTZ,
    resolver        TEXT,
    note            TEXT
);

-- 6. budget_reservation — Reserve/Commit/Release lifecycle
CREATE TABLE budget_reservation (
    id              UUID PRIMARY KEY,
    trace_id        UUID NOT NULL,
    tenant_id       TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    team_id         TEXT NOT NULL,
    estimated_cents INTEGER NOT NULL,
    actual_cents    INTEGER,
    state           TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    settled_at      TIMESTAMPTZ
);
CREATE INDEX ON budget_reservation (state, created_at);
CREATE INDEX ON budget_reservation (tenant_id, team_id, created_at DESC);

-- budget_team_monthly_v — materialized view for dashboard
CREATE VIEW budget_team_monthly_v AS
SELECT
    tenant_id, team_id,
    date_trunc('month', created_at) AS month,
    SUM(CASE WHEN state IN ('reserved','committed') THEN
        COALESCE(actual_cents, estimated_cents) ELSE 0 END) AS used_cents
FROM budget_reservation
GROUP BY tenant_id, team_id, month;

-- 7. raw_access_audit — P2+ access audit (P0 schema-only, inert)
CREATE TABLE raw_access_audit (
    id              BIGSERIAL PRIMARY KEY,
    accessed_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    accessor_user   TEXT NOT NULL,
    accessor_role   TEXT NOT NULL,
    target_trace_id UUID NOT NULL,
    target_repo_id  TEXT,
    purpose         TEXT,
    break_glass     BOOLEAN NOT NULL DEFAULT FALSE,
    ip              INET,
    user_agent      TEXT
);

-- 8. audit_chain_root — P3+ hash chain daily roots (P0 schema-only, inert)
CREATE TABLE audit_chain_root (
    id              BIGSERIAL PRIMARY KEY,
    tenant_id       TEXT NOT NULL,
    chain_date      DATE NOT NULL,
    merkle_root     BYTEA NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, chain_date)
);
