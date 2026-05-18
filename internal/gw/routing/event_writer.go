package routing

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EventWriter writes routing decisions to the routing_event table.
type EventWriter struct {
	pool *pgxpool.Pool
}

// NewEventWriter creates a new Postgres-backed routing event writer.
func NewEventWriter(pool *pgxpool.Pool) *EventWriter {
	return &EventWriter{pool: pool}
}

// Write inserts a routing event.
func (w *EventWriter) Write(ctx context.Context, traceID, tenantID string, attemptNo int, decision json.RawMessage, poolSelected string, memberSelected json.RawMessage, fallbackChain json.RawMessage, degradedFeatures []string, breakerState string, seed []byte) error {
	_, err := w.pool.Exec(ctx,
		`INSERT INTO routing_event
		 (event_at, trace_id, attempt_no, tenant_id, decision, pool_selected,
		  member_selected, fallback_chain, degraded_features, breaker_state, seed)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		time.Now(), traceID, attemptNo, tenantID, decision, poolSelected,
		memberSelected, fallbackChain, degradedFeatures, breakerState, seed,
	)
	return err
}
