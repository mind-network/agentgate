// Package audit provides the append-only audit event writer.
package audit

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"
)

// EventType enumerates audit event kinds.
type EventType string

const (
	EventRequestReceived    EventType = "request_received"
	EventDecisionMade       EventType = "decision_made"
	EventProviderCall       EventType = "provider_call"
	EventRequestCompleted   EventType = "request_completed"
	EventBudgetSoftWarn     EventType = "budget_soft_warn"
	EventReservationExpired EventType = "reservation_expired"
	EventRoutingNoCandidate EventType = "routing_no_candidate_after_constraints"
	EventProvider5xxRetry   EventType = "provider_5xx_after_retry"
	EventInviteCreated      EventType = "invite.created"
	EventInviteExchanged    EventType = "invite.exchanged"
)

// Event is a single audit log entry.
type Event struct {
	EventAt        time.Time       `json:"event_at"`
	TraceID        string          `json:"trace_id"`
	SessionID      string          `json:"session_id,omitempty"`
	TenantID       string          `json:"tenant_id"`
	UserID         string          `json:"user_id"`
	TeamID         string          `json:"team_id"`
	RepoID         string          `json:"repo_id,omitempty"`
	EventType      EventType       `json:"event_type"`
	Decision       json.RawMessage `json:"decision,omitempty"`
	RuleIDs        []string        `json:"rule_ids,omitempty"`
	Detail         json.RawMessage `json:"detail,omitempty"`
	RequestSummary json.RawMessage `json:"request_summary,omitempty"`
	RoutedTo       string          `json:"routed_to,omitempty"`
	FallbackChain  json.RawMessage `json:"fallback_chain,omitempty"`
	ErrorCode      string          `json:"error_code,omitempty"`
}

// Writer is the append-only audit event writer.
// P0: logs to stdout. P1+: writes to Postgres audit_event table.
type Writer struct {
	events []Event
}

// NewWriter creates a new audit writer.
func NewWriter() *Writer {
	return &Writer{}
}

// Write appends an audit event.
func (w *Writer) Write(ev Event) {
	w.events = append(w.events, ev)
	slog.Info("audit event",
		"event_type", ev.EventType,
		"trace_id", ev.TraceID,
		"user_id", ev.UserID,
		"team_id", ev.TeamID,
	)
}

// Flush returns all buffered events (P0: for test assertions).
func (w *Writer) Flush() []Event {
	out := w.events
	w.events = nil
	return out
}

// NewEvent creates a minimal audit event with the required fields.
func NewEvent(eventType EventType, traceID, tenantID, userID, teamID string) Event {
	return Event{
		EventAt:   time.Now(),
		TraceID:   traceID,
		TenantID:  tenantID,
		UserID:    userID,
		TeamID:    teamID,
		EventType: eventType,
	}
}

// AlertBudget is called from the budget service when usage exceeds 80%.
func (w *Writer) AlertBudget(teamID string, usedCents, capCents int) {
	detail, _ := json.Marshal(map[string]any{
		"team_id":    teamID,
		"used_cents": usedCents,
		"cap_cents":  capCents,
		"pct":        fmt.Sprintf("%d%%", usedCents*100/capCents),
	})
	w.Write(Event{
		EventAt:   time.Now(),
		EventType: EventBudgetSoftWarn,
		TeamID:    teamID,
		Detail:    detail,
	})
}

// AlertReservationExpired is called from the budget settler.
func (w *Writer) AlertReservationExpired(ids []string) {
	detail, _ := json.Marshal(map[string]any{
		"reservation_ids": ids,
		"count":           len(ids),
	})
	w.Write(Event{
		EventAt:   time.Now(),
		EventType: EventReservationExpired,
		Detail:    detail,
	})
}
