-- 0005_cost_breakdown.down.sql — revert per-bucket cost breakdown columns

ALTER TABLE cost_event DROP COLUMN input_cost_cents;
ALTER TABLE cost_event DROP COLUMN output_cost_cents;
ALTER TABLE cost_event DROP COLUMN cache_read_cost_cents;
ALTER TABLE cost_event DROP COLUMN cache_create_cost_cents;
