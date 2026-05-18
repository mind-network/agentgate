-- 0001_init.down.sql — rollback P0 MVP schema
DROP VIEW IF EXISTS budget_team_monthly_v;
DROP TABLE IF EXISTS audit_chain_root CASCADE;
DROP TABLE IF EXISTS raw_access_audit CASCADE;
DROP TABLE IF EXISTS budget_reservation CASCADE;
DROP TABLE IF EXISTS approval_request CASCADE;
DROP TABLE IF EXISTS routing_event CASCADE;
DROP TABLE IF EXISTS cost_event CASCADE;
DROP TABLE IF EXISTS audit_event CASCADE;
DROP TABLE IF EXISTS raw_record CASCADE;
