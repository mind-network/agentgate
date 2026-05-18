package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"agentgate/internal/gw/db"
	"agentgate/internal/gw/edge"
)

// setupTestDB starts a Postgres container, runs migrations, inserts known
// cost_event rows, and returns the pool + a cleanup function.
func setupTestDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("agentgate"),
		postgres.WithUsername("agentgate"),
		postgres.WithPassword("agentgate"),
	)
	if err != nil {
		t.Skipf("testcontainers not available (need Docker): %v", err)
		return nil, func() {}
	}

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}

	if err := db.Migrate(ctx, pool, db.Migrations, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// Insert cost_event rows covering multiple dimensions and dates.
	//
	// event_at layout:
	//   t0 = 2026-01-15 (mid-month, to be in-range for default filter)
	//   t1 = 2026-02-15 (next month)
	//   t2 = 2026-03-15 (outside any realistic "this month" if tested in 2026-04+)
	//   t3 = 2025-12-01 (previous year — outside current month/year filters)
	now := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	lastMonth := now.AddDate(0, -1, 0) // 2025-12-15
	nextMonth := now.AddDate(0, 1, 0)  // 2026-02-15
	future := now.AddDate(0, 2, 0)     // 2026-03-15

	// Row layout: (event_at, trace_id, attempt_no, tenant_id, user_id, team_id, repo_id, task_type,
	//             policy_rule_id, wire, vendor_id, endpoint_id, model, pool, is_private,
	//             input_tokens, output_tokens, cache_read_tokens, cache_create_tokens,
	//             cost_cents, currency, cost_source, latency_ms, success, error_class)

	rows := [][]any{
		// Team alpha — this month, many tokens
		{now, "11111111-1111-1111-1111-111111111111", 1, "tenant-1", "alice", "alpha", "repo-a", "code", "rule-1",
			"anthropic", "anthropic", "ep-1", "claude-opus-4", "premium", false,
			1000, 200, 0, 0, 500, "USD", "api", 200, true, nil},
		{now, "22222222-2222-2222-2222-222222222222", 1, "tenant-1", "alice", "alpha", "repo-a", "code", "rule-1",
			"anthropic", "anthropic", "ep-1", "claude-sonnet-4", "premium", false,
			500, 100, 0, 0, 50, "USD", "api", 150, true, nil},
		// Team beta — this month
		{now, "33333333-3333-3333-3333-333333333333", 1, "tenant-1", "bob", "beta", "repo-b", "review", "rule-2",
			"openai", "openai", "ep-2", "gpt-4o", "standard", false,
			200, 50, 0, 0, 100, "USD", "api", 300, true, nil},
		// Team alpha — failed request
		{now, "44444444-4444-4444-4444-444444444444", 2, "tenant-1", "alice", "alpha", "repo-a", "code", "rule-1",
			"anthropic", "anthropic", "ep-1", "claude-opus-4", "premium", false,
			0, 0, 0, 0, 0, "USD", "api", nil, false, "rate_limited"},
		// Last month — should be outside "this month"
		{lastMonth, "55555555-5555-5555-5555-555555555555", 1, "tenant-1", "alice", "alpha", "repo-a", "code", "rule-1",
			"anthropic", "anthropic", "ep-1", "claude-opus-4", "premium", false,
			100, 20, 0, 0, 50, "USD", "api", 100, true, nil},
		// Next month
		{nextMonth, "66666666-6666-6666-6666-666666666666", 1, "tenant-1", "bob", "beta", "repo-c", "code", "rule-2",
			"openai", "openai", "ep-2", "gpt-4o", "standard", false,
			300, 60, 0, 0, 150, "USD", "api", 200, true, nil},
		// Future month
		{future, "77777777-7777-7777-7777-777777777777", 1, "tenant-1", "carol", "gamma", "repo-c", "code", "rule-3",
			"openai", "openai", "ep-2", "gpt-4o-mini", "standard", false,
			50, 10, 0, 0, 25, "USD", "api", 50, true, nil},
	}

	for i, row := range rows {
		_, err := pool.Exec(ctx, `INSERT INTO cost_event (
			event_at, trace_id, attempt_no, tenant_id, user_id, team_id, repo_id, task_type,
			policy_rule_id, wire, vendor_id, endpoint_id, model, pool, is_private,
			input_tokens, output_tokens, cache_read_tokens, cache_create_tokens,
			cost_cents, currency, cost_source, latency_ms, success, error_class
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,
			$16,$17,$18,$19,$20,$21,$22,$23,$24,$25
		)`, row...)
		if err != nil {
			t.Fatalf("insert row %d: %v", i, err)
		}
	}

	cleanup := func() {
		pool.Close()
		if err := pgContainer.Terminate(context.Background()); err != nil {
			t.Logf("terminate: %v", err)
		}
	}

	return pool, cleanup
}

// TestCostSummaryDefault tests that no-param calls return 200 with JSON array.
func TestCostSummaryDefault(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	h := NewCostSummaryHandler(pool)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}

	var result []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Default dim is "team", so we should see aggregated team rows.
	// The data has alpha (650 + 0 + 50 = 700 cost_cents across 4 events) and beta (100 + 150).
	if len(result) < 2 {
		t.Errorf("expected at least 2 team rows, got %d: %v", len(result), result)
	}
}

// TestCostSummaryDim tests each valid dim grouping.
func TestCostSummaryDim(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	h := NewCostSummaryHandler(pool)

	tests := []struct {
		name       string
		dim        string
		minRows    int
		firstValue string // expected first dim_value (highest cost)
	}{
		{"team", "team", 2, "alpha"},
		{"user", "user", 2, "alice"},
		{"repo", "repo", 2, "repo-a"},
		{"model", "model", 3, "claude-opus-4"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				r.URL.RawQuery = "dim=" + tt.dim
				h.ServeHTTP(w, r)
			}))
			defer srv.Close()

			resp, err := http.Get(srv.URL + "?dim=" + tt.dim)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != 200 {
				t.Errorf("expected 200, got %d", resp.StatusCode)
			}

			var result []map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
				t.Fatalf("decode: %v", err)
			}

			if len(result) < tt.minRows {
				t.Errorf("expected at least %d rows, got %d: %v", tt.minRows, len(result), result)
			}
		})
	}
}

// TestCostSummaryDateRange tests from/to filtering.
func TestCostSummaryDateRange(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	h := NewCostSummaryHandler(pool)
	srv := httptest.NewServer(h)
	defer srv.Close()

	t.Run("only from", func(t *testing.T) {
		// from=2026-02-01 should exclude rows before Feb 2026
		resp, err := http.Get(srv.URL + "?from=2026-02-01")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}

		var result []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode: %v", err)
		}

		// Should only return Feb+ rows (t6=Feb, t7=Mar = 2 rows)
		if len(result) == 0 {
			t.Error("expected non-empty result when filtering from Feb")
		}
	})

	t.Run("only to", func(t *testing.T) {
		// to=2026-01-16 should return only rows before Jan 16
		resp, err := http.Get(srv.URL + "?to=2026-01-16")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}

		var result []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode: %v", err)
		}

		// Should include events on Jan 15 (4 rows for alpha/beta Jan events)
		// The Jan 15 rows have team alpha (3 events) and beta (1 event)
		if len(result) == 0 {
			t.Error("expected non-empty result when filtering to Jan 16")
		}
	})

	t.Run("from and to", func(t *testing.T) {
		// from=2026-02-01, to=2026-02-16 should return only Feb rows
		resp, err := http.Get(srv.URL + "?from=2026-02-01&to=2026-02-16")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != 200 {
			t.Errorf("expected 200, got %d", resp.StatusCode)
		}

		var result []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode: %v", err)
		}

		// Only the Feb 15 row — team beta, model gpt-4o, cost 150
		if len(result) != 1 {
			t.Errorf("expected 1 row for Feb range, got %d: %v", len(result), result)
		}
		if len(result) > 0 {
			cost := int(result[0]["cost_cents"].(float64))
			if cost != 150 {
				t.Errorf("expected cost_cents=150 for Feb, got %d", cost)
			}
		}
	})

	t.Run("exclusive to boundary", func(t *testing.T) {
		// to=2026-01-15 should EXCLUDE Jan 15 rows but INCLUDE
		// rows before Jan 15 (the Dec 15 row).
		resp, err := http.Get(srv.URL + "?to=2026-01-15")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var result []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode: %v", err)
		}

		// Only the Dec 15 alpha row (50 cost_cents) should match.
		if len(result) != 1 {
			t.Errorf("expected 1 row for to=2026-01-15 (exclusive), got %d: %v", len(result), result)
		}
		if len(result) > 0 {
			cost := int(result[0]["cost_cents"].(float64))
			if cost != 50 {
				t.Errorf("expected cost_cents=50 for Dec row, got %d", cost)
			}
		}
	})
}

// TestCostSummaryInvalidDim tests that bad dim values return 400.
func TestCostSummaryInvalidDim(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	h := NewCostSummaryHandler(pool)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, dim := range []string{"project", "  ", "team,user"} {
		t.Run(fmt.Sprintf("dim=%q", dim), func(t *testing.T) {
			resp, err := http.Get(srv.URL + "?dim=" + dim)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != 400 {
				t.Errorf("dim=%q: expected 400, got %d", dim, resp.StatusCode)
			}
		})
	}
}

// TestCostSummaryInvalidDate tests that bad from/to values return 400.
func TestCostSummaryInvalidDate(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	h := NewCostSummaryHandler(pool)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"from-not-a-date", "from=not-a-date"},
		{"from-wrong-format", "from=01-15-2026"},
		{"to-not-a-date", "to=abc"},
		{"to-wrong-format", "to=2026/01/15"},
		{"both-invalid", "from=bad&to=stuff"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, err := http.Get(srv.URL + "?" + tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != 400 {
				t.Errorf("%s: expected 400, got %d: %s", tc.name, resp.StatusCode, tc.query)
			}

			var body map[string]string
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("%s: decode: %v", tc.name, err)
			}
			if body["error"] == "" {
				t.Errorf("%s: expected error message in body", tc.name)
			}
		})
	}
}

// TestCostSummaryAuthContext verifies the handler does not interfere with
// auth middleware context values (set by the middleware and read via edge context).
func TestCostSummaryAuthContext(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	h := NewCostSummaryHandler(pool)

	// Simulate a request through auth middleware by setting context values
	// that edge.GetUserID / edge.GetTeamID / edge.GetRole would provide.
	req := httptest.NewRequest("GET", "/", nil)
	req = req.WithContext(context.WithValue(
		context.WithValue(
			context.WithValue(context.Background(), edge.CtxUserID, "alice"),
			edge.CtxTeamID, "alpha"),
		edge.CtxRole, "developer"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Errorf("expected 200 with auth context, got %d: %s", rec.Code, rec.Body.String())
	}

	var result []map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Auth context should not affect query results — handler should work regardless
	if len(result) < 2 {
		t.Errorf("expected at least 2 rows with auth context, got %d: %v", len(result), result)
	}
}

// TestCostSummaryEmptyDB verifies handler returns [] for an empty DB.
func TestCostSummaryEmptyDB(t *testing.T) {
	ctx := context.Background()

	pgContainer, err := postgres.Run(ctx,
		"postgres:16-alpine",
		postgres.WithDatabase("agentgate"),
		postgres.WithUsername("agentgate"),
		postgres.WithPassword("agentgate"),
	)
	if err != nil {
		t.Skipf("testcontainers not available (need Docker): %v", err)
		return
	}
	defer func() {
		if err := pgContainer.Terminate(ctx); err != nil {
			t.Logf("terminate: %v", err)
		}
	}()

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool, db.Migrations, "migrations"); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	h := NewCostSummaryHandler(pool)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("expected 200 for empty DB, got %d", resp.StatusCode)
	}

	var result []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected empty array for empty DB, got %d rows: %v", len(result), result)
	}
}

// TestCostSummaryResponseShape verifies that each row has the expected fields.
func TestCostSummaryResponseShape(t *testing.T) {
	pool, cleanup := setupTestDB(t)
	defer cleanup()

	h := NewCostSummaryHandler(pool)
	srv := httptest.NewServer(h)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "?dim=model")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var result []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for i, row := range result {
		for _, field := range []string{"bucket", "dim_value", "cost_cents", "input_tokens", "output_tokens", "n_requests", "n_failed"} {
			if _, ok := row[field]; !ok {
				t.Errorf("row %d: missing field %q in %v", i, field, row)
			}
		}
	}

	// Verify sort order: cost descending, then dim ascending
	for i := 1; i < len(result); i++ {
		prevCost := int(result[i-1]["cost_cents"].(float64))
		currCost := int(result[i]["cost_cents"].(float64))
		if currCost > prevCost {
			t.Errorf("rows not sorted by cost descending: row %d cost %d > row %d cost %d",
				i-1, prevCost, i, currCost)
		}
	}
}
