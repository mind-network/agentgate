package audit

import (
	"strings"
	"testing"
)

func TestWriterWritesEvent(t *testing.T) {
	w := NewWriter()
	w.Write(NewEvent(EventRequestReceived, "trace-1", "tenant-1", "user-1", "team-1"))
	w.Write(NewEvent(EventDecisionMade, "trace-1", "tenant-1", "user-1", "team-1"))
	w.Write(NewEvent(EventRequestCompleted, "trace-1", "tenant-1", "user-1", "team-1"))

	events := w.Flush()
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	if events[0].EventType != EventRequestReceived {
		t.Errorf("first event = %s", events[0].EventType)
	}
	if events[1].EventType != EventDecisionMade {
		t.Errorf("second event = %s", events[1].EventType)
	}
}

func TestWriterAlertBudget(t *testing.T) {
	w := NewWriter()
	w.AlertBudget("team-1", 900, 1000)

	events := w.Flush()
	if len(events) != 1 {
		t.Fatalf("expected 1 alert event, got %d", len(events))
	}
	if events[0].EventType != EventBudgetSoftWarn {
		t.Errorf("expected budget_soft_warn, got %s", events[0].EventType)
	}
	if !strings.Contains(string(events[0].Detail), "90%") {
		t.Errorf("detail should contain percentage, got %s", string(events[0].Detail))
	}
	if events[0].TeamID != "team-1" {
		t.Errorf("team_id = %q", events[0].TeamID)
	}
}

func TestWriterAlertReservationExpired(t *testing.T) {
	w := NewWriter()
	w.AlertReservationExpired([]string{"id-1", "id-2"})

	events := w.Flush()
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].EventType != EventReservationExpired {
		t.Errorf("expected reservation_expired, got %s", events[0].EventType)
	}
}

func TestWriterFlushClears(t *testing.T) {
	w := NewWriter()
	w.Write(NewEvent(EventRequestReceived, "t1", "t1", "u1", "t1"))
	if len(w.Flush()) != 1 {
		t.Fatal("expected 1 after flush")
	}
	if len(w.Flush()) != 0 {
		t.Fatal("expected 0 after second flush")
	}
}
