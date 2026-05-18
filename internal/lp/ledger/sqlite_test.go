package ledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempDB(t *testing.T) (*Ledger, string) {
	t.Helper()
	dir := t.TempDir()
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l, dir
}

func insertTrace(t *testing.T, l *Ledger, traceID, model, provider string, tokensIn, tokensOut, costCents int, ts time.Time) {
	t.Helper()
	err := l.Insert(&TraceRecord{
		TraceID:    traceID,
		SessionID:  "sess-1",
		CostCents:  costCents,
		TokensIn:   tokensIn,
		TokensOut:  tokensOut,
		Model:      model,
		Provider:   provider,
		TaskType:   "chat",
		RecordedAt: ts,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
}

func TestOpenAndMigrate(t *testing.T) {
	l, dir := tempDB(t)
	if _, err := os.Stat(filepath.Join(dir, "traces.db")); err != nil {
		t.Fatalf("traces.db not created: %v", err)
	}
	// Re-open should succeed (migration is idempotent).
	_ = l.Close()
	l2, err := Open(dir)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	_ = l2.Close()
}

func TestInsertAndQueryByTraceID(t *testing.T) {
	l, _ := tempDB(t)
	now := time.Now()
	insertTrace(t, l, "trace-abc", "claude-sonnet-4-6", "anthropic", 7, 3, 0, now)

	rec, err := l.QueryByTraceID("trace-abc")
	if err != nil {
		t.Fatalf("QueryByTraceID: %v", err)
	}
	if rec.TraceID != "trace-abc" {
		t.Errorf("trace_id = %q, want trace-abc", rec.TraceID)
	}
	if rec.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q", rec.Model)
	}
	if rec.TokensIn != 7 || rec.TokensOut != 3 {
		t.Errorf("tokens = %d/%d, want 7/3", rec.TokensIn, rec.TokensOut)
	}
}

func TestQueryByTraceIDNotFound(t *testing.T) {
	l, _ := tempDB(t)
	_, err := l.QueryByTraceID("no-such-trace")
	if err == nil {
		t.Fatal("expected error for missing trace")
	}
}

func TestListRecentOrderAndLimit(t *testing.T) {
	l, _ := tempDB(t)
	now := time.Now()

	// Insert 6 traces with staggered timestamps, oldest first.
	for i := 0; i < 6; i++ {
		ts := now.Add(-time.Duration(5-i) * time.Minute)
		insertTrace(t, l, "trace-"+string(rune('0'+i)), "model-"+string(rune('0'+i)), "p", 1, 1, 0, ts)
	}

	// Default limit.
	recs, err := l.ListRecent(0)
	if err != nil {
		t.Fatalf("ListRecent(0): %v", err)
	}
	if len(recs) != 5 {
		t.Fatalf("ListRecent(0) = %d records, want 5", len(recs))
	}
	// Newest first: trace-5 should be first.
	if recs[0].TraceID != "trace-5" {
		t.Errorf("first = %q, want trace-5", recs[0].TraceID)
	}
	if recs[4].TraceID != "trace-1" {
		t.Errorf("last = %q, want trace-1", recs[4].TraceID)
	}

	// Custom limit.
	recs3, err := l.ListRecent(3)
	if err != nil {
		t.Fatalf("ListRecent(3): %v", err)
	}
	if len(recs3) != 3 {
		t.Fatalf("ListRecent(3) = %d records, want 3", len(recs3))
	}
	if recs3[0].TraceID != "trace-5" {
		t.Errorf("first = %q, want trace-5", recs3[0].TraceID)
	}
}

func TestListRecentEmpty(t *testing.T) {
	l, _ := tempDB(t)
	recs, err := l.ListRecent(5)
	if err != nil {
		t.Fatalf("ListRecent on empty: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("expected 0 records, got %d", len(recs))
	}
}

func TestListRecentExceedsCount(t *testing.T) {
	l, _ := tempDB(t)
	now := time.Now()
	insertTrace(t, l, "trace-a", "m", "p", 1, 1, 0, now)

	// Request more than available.
	recs, err := l.ListRecent(10)
	if err != nil {
		t.Fatalf("ListRecent(10): %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("ListRecent(10) = %d records, want 1", len(recs))
	}
}
