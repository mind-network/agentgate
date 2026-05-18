// Package budget implements the Reserve/Commit/Release three-phase
// budget lifecycle (§9.5, §12.5).
package budget

import (
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// State is the lifecycle state of a budget reservation.
type State string

const (
	StateReserved  State = "reserved"
	StateCommitted State = "committed"
	StateReleased  State = "released"
	StateExpired   State = "expired"
)

// Reservation tracks a single budget reservation.
type Reservation struct {
	ID             uuid.UUID
	TraceID        string
	TenantID       string
	UserID         string
	TeamID         string
	EstimatedCents int
	ActualCents    *int
	State          State
	CreatedAt      time.Time
	SettledAt      *time.Time
}

// Service provides the budget Reserve/Commit/Release interface.
type Service struct {
	mu           sync.Mutex
	reservations map[uuid.UUID]*Reservation
	teams        map[string]int // team_id → monthly cap cents
	used         map[string]int // team_id → used cents (outstanding + committed)
	alertFn      func(teamID string, usedCents, capCents int)
}

// NewService creates a new budget service.
func NewService() *Service {
	return &Service{
		reservations: make(map[uuid.UUID]*Reservation),
		teams:        make(map[string]int),
		used:         make(map[string]int),
	}
}

// SetTeamCap sets the monthly cap for a team.
func (s *Service) SetTeamCap(teamID string, capCents int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.teams[teamID] = capCents
}

// OnAlert registers a callback invoked when a team exceeds 80% of their cap.
func (s *Service) OnAlert(fn func(teamID string, usedCents, capCents int)) {
	s.alertFn = fn
}

// Reserve creates a budget reservation. P0: never returns error; caps are soft-warn only.
func (s *Service) Reserve(traceID, tenantID, userID, teamID string, estimatedCents int) (*Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

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
	s.reservations[id] = r
	s.used[teamID] += estimatedCents

	// Soft warn if > 80% cap.
	if cap, ok := s.teams[teamID]; ok && cap > 0 {
		if s.used[teamID]*100/cap >= 80 && s.alertFn != nil {
			s.alertFn(teamID, s.used[teamID], cap)
		}
	}
	// P0: never returns ErrHardCapExceeded.
	return r, nil
}

// Commit marks a reservation as committed with actual cost.
func (s *Service) Commit(id uuid.UUID, actualCents int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.reservations[id]
	if !ok {
		return fmt.Errorf("reservation %s not found", id)
	}
	if r.State != StateReserved {
		return fmt.Errorf("reservation %s is %s, cannot commit", id, r.State)
	}

	// Adjust used: subtract estimate, add actual.
	s.used[r.TeamID] -= r.EstimatedCents
	s.used[r.TeamID] += actualCents

	r.ActualCents = &actualCents
	r.State = StateCommitted
	now := time.Now()
	r.SettledAt = &now
	return nil
}

// Release frees a reservation. Called on error or cancellation.
func (s *Service) Release(id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	r, ok := s.reservations[id]
	if !ok {
		return fmt.Errorf("reservation %s not found", id)
	}
	if r.State == StateReleased || r.State == StateExpired {
		return nil // idempotent
	}

	s.used[r.TeamID] -= r.EstimatedCents
	r.State = StateReleased
	now := time.Now()
	r.SettledAt = &now
	return nil
}

// GetReservation returns a reservation by ID.
func (s *Service) GetReservation(id uuid.UUID) (*Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.reservations[id]
	if !ok {
		return nil, fmt.Errorf("reservation %s not found", id)
	}
	return r, nil
}

// TeamUsed returns the used cents for a team (outstanding + committed).
func (s *Service) TeamUsed(teamID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.used[teamID]
}

// ExpiredReservations returns all reservations that are still reserved and
// older than the given duration.
func (s *Service) ExpiredReservations(maxAge time.Duration) []*Reservation {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	var expired []*Reservation
	for _, r := range s.reservations {
		if r.State == StateReserved && r.CreatedAt.Before(cutoff) {
			expired = append(expired, r)
		}
	}
	return expired
}

// ReleaseExpired releases the given set of reservations and returns their IDs.
func (s *Service) ReleaseExpired(maxAge time.Duration) []uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	var released []uuid.UUID
	for _, r := range s.reservations {
		if r.State == StateReserved && r.CreatedAt.Before(cutoff) {
			s.used[r.TeamID] -= r.EstimatedCents
			r.State = StateExpired
			now := time.Now()
			r.SettledAt = &now
			released = append(released, r.ID)
		}
	}
	return released
}
