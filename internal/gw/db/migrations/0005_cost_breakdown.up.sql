-- 0005_cost_breakdown.up.sql — add per-bucket cost breakdown columns
-- Adds input_cost_cents, output_cost_cents, cache_read_cost_cents, cache_create_cost_cents
-- to cost_event so individual bucket costs are persisted alongside the total cost_cents.
-- Existing rows get 0 via DEFAULT.
-- cost_cents remains the authoritative total (sum of all four buckets).

ALTER TABLE cost_event ADD COLUMN input_cost_cents       INTEGER NOT NULL DEFAULT 0;
ALTER TABLE cost_event ADD COLUMN output_cost_cents      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE cost_event ADD COLUMN cache_read_cost_cents   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE cost_event ADD COLUMN cache_create_cost_cents INTEGER NOT NULL DEFAULT 0;
