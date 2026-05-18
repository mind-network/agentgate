-- 0002_api_keys.up.sql — api_keys + setup_tokens tables
-- See HANDOFF-002 § T1 schema.

CREATE TABLE api_keys (
    key_hash TEXT PRIMARY KEY,
    user_id TEXT NOT NULL,
    team_id TEXT NOT NULL,
    role TEXT NOT NULL CHECK (role IN ('developer','team_admin','platform_admin')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ
);
CREATE INDEX api_keys_user_team_idx ON api_keys (user_id, team_id)
    WHERE deleted_at IS NULL;

CREATE TABLE setup_tokens (
    token_hash TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    consumed_at TIMESTAMPTZ
);
