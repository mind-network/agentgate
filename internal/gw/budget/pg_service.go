package budget

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ServiceIface is the interface for budget operations.
type ServiceIface interface {
	Reserve(traceID, tenantID, userID, teamID string, estimatedCents int) (*Reservation, error)
	Commit(id uuid.UUID, actualCents int) error
	Release(id uuid.UUID) error
	GetReservation(id uuid.UUID) (*Reservation, error)
	TeamUsed(teamID string) int
	SetTeamCap(teamID string, capCents int)
	OnAlert(fn func(teamID string, usedCents, capCents int))
	ExpiredReservations(maxAge time.Duration) []*Reservation
	ReleaseExpired(maxAge time.Duration) []uuid.UUID
}

// PgService implements ServiceIface backed by the budget_reservation table.
type PgService struct {
	pool    *pgxpool.Pool
	alertFn func(teamID string, usedCents, capCents int)
	teams   map[string]int // team_id → monthly cap cents (cached)
	// P0: soft-warn only; caps are advisory.
}

// NewPgService creates a new Postgres-backed budget service.
func NewPgService(pool *pgxpool.Pool) *PgService {
	return &PgService{
		pool:  pool,
		teams: make(map[string]int),
	}
}

// SetTeamCap sets the monthly cap for a team.
func (s *PgService) SetTeamCap(teamID string, capCents int) {
	s.teams[teamID] = capCents
}

// OnAlert registers a callback for budget warnings.
func (s *PgService) OnAlert(fn func(teamID string, usedCents, capCents int)) {
	s.alertFn = fn
}

// Reserve creates a budget reservation in the database.
func (s *PgService) Reserve(ctx context.Context, traceID, tenantID, userID, teamID string, estimatedCents int) (*Reservation, error) {
	id := uuid.New()
	r := &Reservation{
		ID:             id,
		TraceID:        traceID,
		TenantID:       tenantID,
		UserID:         userID,
		TeamID:         teamID,
		EstimatedCents: estimatedCents,
		State:          StateReserved,
		CreatedAt:      time.Now(),
	}

	_, err := s.pool.Exec(ctx,
		`INSERT INTO budget_reservation
		 (id, trace_id, tenant_id, user_id, team_id, estimated_cents, state, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		id, traceID, tenantID, userID, teamID, estimatedCents, string(StateReserved), r.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("budget reserve: %w", err)
	}

	// Soft warn if > 80% cap.
	if cap, ok := s.teams[teamID]; ok && cap > 0 {
		used := s.TeamUsed(ctx, teamID)
		if used*100/cap >= 80 && s.alertFn != nil {
			s.alertFn(teamID, used, cap)
		}
	}

	return r, nil
}

// Commit marks a reservation as committed.
func (s *PgService) Commit(ctx context.Context, id uuid.UUID, actualCents int) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE budget_reservation
		 SET state = $1, actual_cents = $2, settled_at = $3
		 WHERE id = $4 AND state = $5`,
		string(StateCommitted), actualCents, time.Now(), id, string(StateReserved))
	if err != nil {
		return fmt.Errorf("budget commit: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("reservation %s not found or not in reserved state", id)
	}
	return nil
}

// Release frees a reservation.
func (s *PgService) Release(ctx context.Context, id uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE budget_reservation
		 SET state = $1, settled_at = $2
		 WHERE id = $3 AND state NOT IN ($4,$5)`,
		string(StateReleased), time.Now(), id, string(StateReleased), string(StateExpired))
	return err
}

// GetReservation retrieves a reservation by ID.
func (s *PgService) GetReservation(ctx context.Context, id uuid.UUID) (*Reservation, error) {
	var r Reservation
	var actualCents *int
	var settledAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT id, trace_id, tenant_id, user_id, team_id, estimated_cents,
		        actual_cents, state, created_at, settled_at
		 FROM budget_reservation WHERE id = $1`, id).
		Scan(&r.ID, &r.TraceID, &r.TenantID, &r.UserID, &r.TeamID,
			&r.EstimatedCents, &actualCents, &r.State, &r.CreatedAt, &settledAt)
	if err != nil {
		return nil, fmt.Errorf("reservation %s not found", id)
	}
	if actualCents != nil {
		r.ActualCents = actualCents
	}
	r.SettledAt = settledAt
	return &r, nil
}

// TeamUsed returns used cents for a team (outstanding + committed).
func (s *PgService) TeamUsed(ctx context.Context, teamID string) int {
	var used int
	_ = s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(COALESCE(actual_cents, estimated_cents)), 0)
		 FROM budget_reservation
		 WHERE team_id = $1 AND state IN ($2,$3)`,
		teamID, string(StateReserved), string(StateCommitted)).Scan(&used)
	return used
}

// ExpiredReservations returns all reserved reservations older than maxAge.
func (s *PgService) ExpiredReservations(ctx context.Context, maxAge time.Duration) []*Reservation {
	cutoff := time.Now().Add(-maxAge)
	rows, err := s.pool.Query(ctx,
		`SELECT id, trace_id, tenant_id, user_id, team_id, estimated_cents, state, created_at
		 FROM budget_reservation
		 WHERE state = $1 AND created_at < $2`,
		string(StateReserved), cutoff)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var result []*Reservation
	for rows.Next() {
		var r Reservation
		if err := rows.Scan(&r.ID, &r.TraceID, &r.TenantID, &r.UserID, &r.TeamID,
			&r.EstimatedCents, &r.State, &r.CreatedAt); err != nil {
			continue
		}
		result = append(result, &r)
	}
	return result
}

// ReleaseExpired expires and releases stale reservations.
func (s *PgService) ReleaseExpiredCtx(ctx context.Context, maxAge time.Duration) []uuid.UUID {
	cutoff := time.Now().Add(-maxAge)
	rows, err := s.pool.Query(ctx,
		`UPDATE budget_reservation
		 SET state = $1, settled_at = $2
		 WHERE state = $3 AND created_at < $4
		 RETURNING id`,
		string(StateExpired), time.Now(), string(StateReserved), cutoff)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// ReleaseExpired adapter (no context) for Settlable interface.
func (s *PgService) ReleaseExpired(maxAge time.Duration) []uuid.UUID {
	return s.ReleaseExpiredCtx(context.Background(), maxAge)
}

