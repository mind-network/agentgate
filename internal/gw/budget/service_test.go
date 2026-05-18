package budget

import (
	"testing"
	"time"
)

func TestReserveCommit(t *testing.T) {
	svc := NewService()
	svc.SetTeamCap("team-1", 10000)

	r, err := svc.Reserve("trace-1", "tenant-1", "user-1", "team-1", 500)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if r.State != StateReserved {
		t.Errorf("expected reserved, got %s", r.State)
	}
	if svc.TeamUsed("team-1") != 500 {
		t.Errorf("expected 500 used, got %d", svc.TeamUsed("team-1"))
	}

	if err := svc.Commit(r.ID, 480); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if svc.TeamUsed("team-1") != 480 {
		t.Errorf("expected 480 used after commit (500-500+480), got %d", svc.TeamUsed("team-1"))
	}

	r2, _ := svc.GetReservation(r.ID)
	if r2.State != StateCommitted {
		t.Errorf("expected committed, got %s", r2.State)
	}
}

func TestReserveRelease(t *testing.T) {
	svc := NewService()
	svc.SetTeamCap("team-1", 10000)

	r, _ := svc.Reserve("trace-1", "tenant-1", "user-1", "team-1", 300)
	_ = svc.Release(r.ID)

	if svc.TeamUsed("team-1") != 0 {
		t.Errorf("expected 0 used after release, got %d", svc.TeamUsed("team-1"))
	}

	r2, _ := svc.GetReservation(r.ID)
	if r2.State != StateReleased {
		t.Errorf("expected released, got %s", r2.State)
	}
}

func TestReserveNeverHardCap(t *testing.T) {
	svc := NewService()
	svc.SetTeamCap("team-1", 1000)

	// Reserve far beyond cap — P0 must not error.
	_, err := svc.Reserve("trace-1", "tenant-1", "user-1", "team-1", 50000)
	if err != nil {
		t.Fatalf("P0 Reserve should never return error (soft-warn only): %v", err)
	}
}

func TestReserveAlertAt80Percent(t *testing.T) {
	svc := NewService()
	svc.SetTeamCap("team-1", 1000)

	var alerted bool
	svc.OnAlert(func(teamID string, used, cap int) {
		alerted = true
		if teamID != "team-1" || used < 800 || cap != 1000 {
			t.Errorf("alert params: team=%s used=%d cap=%d", teamID, used, cap)
		}
	})

	// Reserve 900 (90% of 1000 cap) — should trigger alert.
	_, _ = svc.Reserve("trace-1", "tenant-1", "user-1", "team-1", 900)
	if !alerted {
		t.Error("expected alert at >=80% cap usage")
	}
}

func TestSettlerExpiredReservations(t *testing.T) {
	svc := NewService()
	svc.SetTeamCap("team-1", 10000)

	// Create reservations that should expire.
	r1, _ := svc.Reserve("trace-1", "t1", "u1", "team-1", 100)
	r2, _ := svc.Reserve("trace-2", "t1", "u1", "team-1", 200)

	// Simulate time passing by manually releasing them.
	expired := svc.ReleaseExpired(0) // maxAge=0 means everything is expired
	if len(expired) != 2 {
		t.Errorf("expected 2 expired, got %d", len(expired))
	}

	if svc.TeamUsed("team-1") != 0 {
		t.Errorf("expected 0 used after expiry, got %d", svc.TeamUsed("team-1"))
	}
	_ = r1
	_ = r2
}

func TestCommitNonReservedFails(t *testing.T) {
	svc := NewService()
	r, _ := svc.Reserve("trace-1", "t1", "u1", "team-1", 100)
	_ = svc.Commit(r.ID, 100)

	// Second commit should fail.
	err := svc.Commit(r.ID, 100)
	if err == nil {
		t.Fatal("expected error on double commit")
	}
}

func TestReleaseIdempotent(t *testing.T) {
	svc := NewService()
	r, _ := svc.Reserve("trace-1", "t1", "u1", "team-1", 100)
	_ = svc.Release(r.ID)
	// Second release should be no-op.
	if err := svc.Release(r.ID); err != nil {
		t.Errorf("expected idempotent release, got: %v", err)
	}
}

func TestSettlerRunOnce(t *testing.T) {
	svc := NewService()
	svc.SetTeamCap("team-1", 10000)

	_, _ = svc.Reserve("trace-old", "t1", "u1", "team-1", 500)

	s := NewSettler(svc)
	// With maxAge=0, all reserved are immediately expired.
	released := s.RunOnce(0)
	if len(released) != 1 {
		t.Errorf("expected 1 released, got %d", len(released))
	}
	if used := svc.TeamUsed("team-1"); used != 0 {
		t.Errorf("expected 0 used after settler run, got %d", used)
	}
}

func TestNewServiceUsesTime(t *testing.T) {
	svc := NewService()
	r, _ := svc.Reserve("trace-1", "t1", "u1", "team-1", 100)
	if r.CreatedAt.After(time.Now()) {
		t.Error("created_at should not be in the future")
	}
	if r.CreatedAt.Before(time.Now().Add(-time.Minute)) {
		t.Error("created_at should be recent")
	}
}

func TestExpiredReservationsByMaxAge(t *testing.T) {
	svc := NewService()
	r, _ := svc.Reserve("trace-1", "t1", "u1", "team-1", 100)

	// With maxAge=1 hour, a just-created reservation is not expired.
	expired := svc.ExpiredReservations(time.Hour)
	if len(expired) != 0 {
		t.Errorf("expected 0 expired with 1h maxAge, got %d", len(expired))
	}

	// With maxAge=0, all are expired.
	expired = svc.ExpiredReservations(0)
	if len(expired) != 1 {
		t.Errorf("expected 1 expired with 0 maxAge, got %d", len(expired))
	}
	_ = r
}
