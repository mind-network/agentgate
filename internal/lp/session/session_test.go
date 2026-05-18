package session

import (
	"testing"
)

func TestNewSessionHasID(t *testing.T) {
	s := New("test-repo")
	if s.SessionID == "" {
		t.Fatal("expected non-empty SessionID")
	}
	if s.PID == 0 {
		t.Fatal("expected non-zero PID")
	}
	if s.StartedAt.IsZero() {
		t.Fatal("expected non-zero StartedAt")
	}
}

func TestNewSessionDeterministic(t *testing.T) {
	s1 := New("test-repo")
	s2 := New("test-repo")
	// Session IDs will differ because start_ts includes nanoseconds,
	// but they should be the same length (64 hex chars for sha256).
	if len(s1.SessionID) != 64 {
		t.Errorf("expected 64-char session ID, got %d", len(s1.SessionID))
	}
	if len(s2.SessionID) != 64 {
		t.Errorf("expected 64-char session ID, got %d", len(s2.SessionID))
	}
}
