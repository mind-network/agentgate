// Package ledger manages the local SQLite trace database (~/.aicg/traces.db).
package ledger

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// TraceRecord stores parsed aicg.usage event data locally.
type TraceRecord struct {
	TraceID    string
	SessionID  string
	CostCents  int
	TokensIn   int
	TokensOut  int
	Model      string
	Provider   string
	TaskType   string
	RecordedAt time.Time
}

// Ledger wraps the SQLite connection for local trace storage.
type Ledger struct {
	db *sql.DB
}

// Open opens or creates the ledger database.
func Open(dir string) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create ledger dir: %w", err)
	}
	path := filepath.Join(dir, "traces.db")
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	l := &Ledger{db: db}
	if err := l.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return l, nil
}

func (l *Ledger) migrate() error {
	_, err := l.db.Exec(`
		CREATE TABLE IF NOT EXISTS traces (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			trace_id TEXT NOT NULL,
			session_id TEXT,
			cost_cents INTEGER NOT NULL DEFAULT 0,
			tokens_in INTEGER NOT NULL DEFAULT 0,
			tokens_out INTEGER NOT NULL DEFAULT 0,
			model TEXT,
			provider TEXT,
			task_type TEXT,
			recorded_at TEXT NOT NULL DEFAULT (datetime('now'))
		);
		CREATE INDEX IF NOT EXISTS idx_traces_trace_id ON traces(trace_id);
		CREATE INDEX IF NOT EXISTS idx_traces_recorded_at ON traces(recorded_at);
	`)
	return err
}

// Insert writes a trace record to the ledger.
func (l *Ledger) Insert(r *TraceRecord) error {
	_, err := l.db.Exec(
		`INSERT INTO traces (trace_id, session_id, cost_cents, tokens_in, tokens_out, model, provider, task_type, recorded_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.TraceID, r.SessionID, r.CostCents, r.TokensIn, r.TokensOut,
		r.Model, r.Provider, r.TaskType, r.RecordedAt.Format(time.RFC3339),
	)
	return err
}

// QueryByTraceID returns the trace record for a given trace_id.
func (l *Ledger) QueryByTraceID(traceID string) (*TraceRecord, error) {
	row := l.db.QueryRow(
		`SELECT trace_id, session_id, cost_cents, tokens_in, tokens_out, model, provider, task_type, recorded_at
		 FROM traces WHERE trace_id = ? LIMIT 1`, traceID,
	)
	r := &TraceRecord{}
	var recordedAt string
	err := row.Scan(&r.TraceID, &r.SessionID, &r.CostCents, &r.TokensIn, &r.TokensOut,
		&r.Model, &r.Provider, &r.TaskType, &recordedAt)
	if err != nil {
		return nil, err
	}
	r.RecordedAt, _ = time.Parse(time.RFC3339, recordedAt)
	return r, nil
}

// ListRecent returns the most recent trace records, ordered newest first, up to limit.
func (l *Ledger) ListRecent(limit int) ([]*TraceRecord, error) {
	if limit <= 0 {
		limit = 5
	}
	rows, err := l.db.Query(
		`SELECT trace_id, session_id, cost_cents, tokens_in, tokens_out, model, provider, task_type, recorded_at
		 FROM traces ORDER BY id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list recent traces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var records []*TraceRecord
	for rows.Next() {
		r := &TraceRecord{}
		var recordedAt string
		if err := rows.Scan(&r.TraceID, &r.SessionID, &r.CostCents, &r.TokensIn, &r.TokensOut,
			&r.Model, &r.Provider, &r.TaskType, &recordedAt); err != nil {
			return nil, fmt.Errorf("scan trace: %w", err)
		}
		r.RecordedAt, _ = time.Parse(time.RFC3339, recordedAt)
		records = append(records, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate traces: %w", err)
	}
	return records, nil
}

// Close closes the database.
func (l *Ledger) Close() error {
	return l.db.Close()
}
