package cost

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PgWriter writes cost events to the cost_event table.
type PgWriter struct {
	pool *pgxpool.Pool
}

// NewPgWriter creates a new Postgres-backed cost writer.
func NewPgWriter(pool *pgxpool.Pool) *PgWriter {
	return &PgWriter{pool: pool}
}

// Write inserts a cost event into the cost_event table.
func (w *PgWriter) Write(ctx context.Context, ev *Event, traceID, tenantID, userID, teamID, repoID, taskType, policyRuleID string, attemptNo int, latencyMs int, success bool, errorClass string) error {
	_, err := w.pool.Exec(ctx,
		`INSERT INTO cost_event
		 (event_at, trace_id, attempt_no, tenant_id, user_id, team_id, repo_id,
		  task_type, policy_rule_id, wire, vendor_id, endpoint_id, model, pool,
		  is_private, input_tokens, output_tokens, cache_read_tokens, cache_create_tokens,
		  cost_cents, currency, cost_source, latency_ms, success, error_class,
		  input_cost_cents, output_cost_cents, cache_read_cost_cents, cache_create_cost_cents)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29)`,
		time.Now(), traceID, attemptNo, tenantID, userID, teamID, repoID,
		taskType, policyRuleID, ev.Wire, ev.VendorID, ev.EndpointID, ev.Model, ev.Pool,
		ev.IsPrivate, ev.InputTokens, ev.OutputTokens, ev.CacheReadTokens, ev.CacheCreateTokens,
		ev.CostCents, ev.Currency, ev.CostSource, latencyMs, success, errorClass,
		ev.InputCostCents, ev.OutputCostCents, ev.CacheReadCostCents, ev.CacheCreateCostCents,
	)
	return err
}
