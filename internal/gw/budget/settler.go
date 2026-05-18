package budget

import (
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// Settlable is the interface the Settler needs from a budget service.
type Settlable interface {
	ReleaseExpired(maxAge time.Duration) []uuid.UUID
}

// Settler periodically scans for expired reservations and releases them.
// P0: scans every minute for reservations older than 10 minutes.
type Settler struct {
	service Settlable
	done    chan struct{}
	onAudit func(reservationIDs []uuid.UUID)
}

// NewSettler creates a settler for the given budget service.
func NewSettler(service Settlable) *Settler {
	return &Settler{
		service: service,
		done:    make(chan struct{}),
	}
}

// OnAudit registers a callback invoked when reservations expire, for audit logging.
func (s *Settler) OnAudit(fn func(reservationIDs []uuid.UUID)) {
	s.onAudit = fn
}

// Start begins the settler loop. The interval and maxAge parameters
// control how often to scan and the expiry threshold.
func (s *Settler) Start(interval, maxAge time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-s.done:
				return
			case <-ticker.C:
				expired := s.service.ReleaseExpired(maxAge)
				if len(expired) > 0 {
					slog.Info("budget settler: released expired reservations", "count", len(expired))
					if s.onAudit != nil {
						s.onAudit(expired)
					}
				}
			}
		}
	}()
}

// Stop shuts down the settler.
func (s *Settler) Stop() {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

// RunOnce is a convenience method for manual settlement (e.g., in tests).
func (s *Settler) RunOnce(maxAge time.Duration) []uuid.UUID {
	return s.service.ReleaseExpired(maxAge)
}

