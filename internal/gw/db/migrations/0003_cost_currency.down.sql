-- 0003_cost_currency.down.sql — revert currency column

ALTER TABLE cost_event DROP COLUMN currency;
