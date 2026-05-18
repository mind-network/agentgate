package audit

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WriterIface is the interface for audit event writing.
type WriterIface interface {
	Write(ev Event)
}

// PgWriter writes audit events to the audit_event table.
type PgWriter struct {
	pool *pgxpool.Pool
}

// NewPgWriter creates a new Postgres-backed audit writer.
func NewPgWriter(pool *pgxpool.Pool) *PgWriter {
	return &PgWriter{pool: pool}
}

// WriteCtx inserts an audit event into the audit_event table with context.
func (w *PgWriter) WriteCtx(ctx context.Context, ev Event) error {
	_, err := w.pool.Exec(ctx,
		`INSERT INTO audit_event
		 (event_at, trace_id, session_id, tenant_id, user_id, team_id, repo_id,
		  event_type, decision, rule_ids, detail, request_summary, routed_to,
		  fallback_chain, error_code)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		ev.EventAt, ev.TraceID, ev.SessionID, ev.TenantID, ev.UserID, ev.TeamID, ev.RepoID,
		ev.EventType, ev.Decision, ev.RuleIDs, ev.Detail, ev.RequestSummary, ev.RoutedTo,
		ev.FallbackChain, ev.ErrorCode,
	)
	return err
}

// Write satisfies the WriterIface interface.
func (w *PgWriter) Write(ev Event) {
	_ = w.WriteCtx(context.Background(), ev)
}

// AlertBudget writes a budget soft warn audit event.
func (w *PgWriter) AlertBudget(ctx context.Context, teamID string, usedCents, capCents int) error {
	detail, _ := json.Marshal(map[string]any{
		"team_id":    teamID,
		"used_cents": usedCents,
		"cap_cents":  capCents,
		"pct":        usedCents * 100 / capCents,
	})
	ev := NewEvent(EventBudgetSoftWarn, "", "", "", teamID)
	ev.Detail = detail
	return w.WriteCtx(ctx, ev)
}

// Ensure PgWriter implements WriterIface.
var _ WriterIface = (*PgWriter)(nil)
