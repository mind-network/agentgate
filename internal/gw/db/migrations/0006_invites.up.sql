-- 0006_invites.up.sql — invites table for one-shot onboarding tokens
-- See HANDOFF-023 § Context (R-18, §13.1 / §13.8).
--
-- An invite_token is minted by a platform_admin and consumed exactly once by
-- an LP at /api/v1/lp/exchange-invite. The cleartext token is never stored;
-- only its sha256 hex hash. A partial unique index on (token_hash) WHERE used_at IS NULL
-- enforces "at most one unused row per token_hash"; the table-level UNIQUE on
-- token_hash retains uniqueness across the row lifetime.

CREATE TABLE invites (
    id              BIGSERIAL PRIMARY KEY,
    token_hash      TEXT NOT NULL UNIQUE,
    role            TEXT NOT NULL CHECK (role IN ('developer','team_admin','platform_admin')),
    team_id         TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at      TIMESTAMPTZ NOT NULL,
    used_at         TIMESTAMPTZ NULL,
    used_by         TEXT NULL,
    machine_id      TEXT NULL
);
CREATE INDEX invites_unused_idx ON invites (token_hash) WHERE used_at IS NULL;
CREATE INDEX invites_created_by_idx ON invites (created_by, created_at DESC);
