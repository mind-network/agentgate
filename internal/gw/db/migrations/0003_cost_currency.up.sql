-- 0003_cost_currency.up.sql — add currency column to cost_event
-- D3/D4: cost_cents semantics expand to "minor unit of this row's currency"
-- Default 'USD' makes existing rows backwards-compatible.

ALTER TABLE cost_event ADD COLUMN currency CHAR(3) NOT NULL DEFAULT 'USD';
