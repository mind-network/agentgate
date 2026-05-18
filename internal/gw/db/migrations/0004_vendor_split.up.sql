-- 0004_vendor_split.up.sql — split provider into wire + vendor_id
-- Greenfield DROP+CREATE per D4: P0 pre-prod, no live customer data.
-- Renames provider → wire; adds vendor_id TEXT NOT NULL.
-- Index (provider, model, event_at DESC) becomes (vendor_id, model, event_at DESC).

DROP TABLE cost_event;

CREATE TABLE cost_event (
    id                  BIGSERIAL PRIMARY KEY,
    event_at            TIMESTAMPTZ NOT NULL,
    trace_id            UUID NOT NULL,
    attempt_no          SMALLINT NOT NULL,
    tenant_id           TEXT NOT NULL,
    user_id             TEXT NOT NULL,
    team_id             TEXT NOT NULL,
    repo_id             TEXT,
    task_type           TEXT,
    policy_rule_id      TEXT,
    wire                TEXT NOT NULL,
    vendor_id           TEXT NOT NULL,
    endpoint_id         TEXT NOT NULL,
    model               TEXT NOT NULL,
    pool                TEXT NOT NULL,
    is_private          BOOLEAN NOT NULL,
    input_tokens        INTEGER NOT NULL,
    output_tokens       INTEGER NOT NULL,
    cache_read_tokens   INTEGER DEFAULT 0,
    cache_create_tokens INTEGER DEFAULT 0,
    cost_cents          INTEGER NOT NULL,
    currency            CHAR(3) NOT NULL DEFAULT 'USD',
    cost_source         TEXT NOT NULL,
    latency_ms          INTEGER,
    success             BOOLEAN NOT NULL,
    error_class         TEXT
);

CREATE INDEX ON cost_event (event_at DESC);
CREATE INDEX ON cost_event (tenant_id, team_id, event_at DESC);
CREATE INDEX ON cost_event (tenant_id, user_id, event_at DESC);
CREATE INDEX ON cost_event (tenant_id, repo_id, event_at DESC);
CREATE INDEX ON cost_event (vendor_id, model, event_at DESC);
