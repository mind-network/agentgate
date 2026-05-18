package server

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"agentgate/internal/gw/audit"
	"agentgate/internal/gw/budget"
	"agentgate/internal/gw/config"
	"agentgate/internal/gw/cost"
	"agentgate/internal/gw/db"
	"agentgate/internal/gw/policy"
	"agentgate/internal/gw/provider"
	"agentgate/internal/gw/routing"
)

// startTestPostgres starts a test Postgres container, runs migrations, and
// returns a connection pool. Skips the test if Docker is not available.
func startTestPostgres(t *testing.T) (*pgxpool.Pool, func()) {
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
		return nil, nil
	}

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	pool, err := db.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Migrate(ctx, pool, db.Migrations, "migrations"); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}

	cleanup := func() {
		pool.Close()
		if err := pgContainer.Terminate(ctx); err != nil {
			t.Logf("terminate postgres: %v", err)
		}
	}
	return pool, cleanup
}

// budgetRow describes a row in budget_reservation for test assertions.
type budgetRow struct {
	State       string
	ActualCents *int
}

// countBudgetReservations returns all rows matching the given trace_id.
func queryBudgetReservations(t *testing.T, pool *pgxpool.Pool, traceID string) []budgetRow {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx,
		`SELECT state, actual_cents FROM budget_reservation WHERE trace_id = $1 ORDER BY created_at`, traceID)
	if err != nil {
		t.Fatalf("query budget_reservation: %v", err)
	}
	defer rows.Close()

	var result []budgetRow
	for rows.Next() {
		var r budgetRow
		if err := rows.Scan(&r.State, &r.ActualCents); err != nil {
			t.Fatalf("scan: %v", err)
		}
		result = append(result, r)
	}
	return result
}

// TestPipelineRunBudgetCommit verifies that a normal EOF through the pipeline
// creates exactly one budget reservation and commits it with actual cost.
func TestPipelineRunBudgetCommit(t *testing.T) {
	pool, cleanup := startTestPostgres(t)
	if pool == nil {
		return // skipped
	}
	defer cleanup()

	bgtSvc := budget.NewPgService(pool)

	// SSE stream with Anthropic usage so the accounted stream produces
	// a real cost event at EOF, triggering PostAccount -> Commit.
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":200,\"output_tokens\":10,\"cache_read_input_tokens\":80,\"cache_creation_input_tokens\":30}}}\n\nevent: content_block_delta\ndata: {\"type\":\"text_delta\",\"text\":\"world\"}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":55}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	adapter := &countingAdapter{name: "anthropic", body: sse}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"anthropic-prod": {Wire: "anthropic", Vendor: "anthropic", URL: "https://api.anthropic.com"},
		},
		Pools: map[string]config.Pool{
			"standard": {
				Members:     []config.PoolMember{{EndpointID: "anthropic-prod", Model: "claude-sonnet-4-6", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, nil)

	pol, _ := policy.NewEngine(&config.PolicyConfig{
		Version: "1.0",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "route", ModelPool: "standard"},
		},
	})

	pricing := &config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor:                      "anthropic",
				Model:                       "claude-sonnet-4-6",
				InputPricePer1KTokens:       3.0,
				OutputPricePer1KTokens:      15.0,
				CacheReadPricePer1KTokens:   float64Ptr(0.30),
				CacheCreatePricePer1KTokens: float64Ptr(3.0),
				Capabilities:                config.ModelCapabilities{Tools: true, CacheControl: true},
			},
		},
	}

	calc := cost.NewCalculator(pricing)
	cfg := &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"anthropic-prod": {ResolvedKey: "test-key"},
		},
	}

	egress := NewEgressPipeline(map[string]provider.Adapter{"anthropic": adapter}, calc, audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), calc, cfg)
	pipeline.PgBudgetSvc = bgtSvc

	traceID := "00000000-0000-0000-0000-000000000001"
	req := &ForwardRequest{
		Envelope: []byte(`{"trace_id":"` + traceID + `"}`),
		Wire: WirePayload{
			Protocol: "anthropic_messages",
			Stream:   true,
			Body:     []byte(`{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`),
		},
	}

	result, err := pipeline.Run(context.Background(), req, traceID)
	if err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}

	// Drain the stream to trigger EOF accounting + PostAccount -> Commit.
	_, _ = io.ReadAll(result.Body)
	_ = result.Body.Close()

	// Verify exactly one budget_reservation row exists for this trace.
	rows := queryBudgetReservations(t, pool, traceID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 budget reservation, got %d", len(rows))
	}
	if rows[0].State != "committed" {
		t.Errorf("reservation state = %q, want committed", rows[0].State)
	}
	if rows[0].ActualCents == nil || *rows[0].ActualCents == 0 {
		t.Error("expected non-zero actual_cents for committed reservation")
	}
}

// errorOnSecondReadAdapter returns a valid body that sends one frame then errors.
type errorOnSecondReadAdapter struct {
	name string
}

func (a *errorOnSecondReadAdapter) Name() string { return a.name }
func (a *errorOnSecondReadAdapter) CountTokens(_ context.Context, _ *provider.ProviderRequest) (int, error) {
	return 0, nil
}
func (a *errorOnSecondReadAdapter) NonStream(_ context.Context, _ *provider.ProviderRequest) ([]byte, error) {
	return nil, nil
}
func (a *errorOnSecondReadAdapter) Stream(_ context.Context, _ *provider.ProviderRequest) (io.ReadCloser, error) {
	// Returns a body that reads one line successfully, then errors.
	return io.NopCloser(&oneLineThenErrorReader{}), nil
}

type oneLineThenErrorReader struct {
	read bool
}

func (r *oneLineThenErrorReader) Read(p []byte) (int, error) {
	if !r.read {
		r.read = true
		return copy(p, "event: ping\ndata: {}\n\n"), nil
	}
	return 0, errors.New("simulated upstream stream failure")
}

// TestPipelineRunBudgetRelease verifies that an upstream stream error
// triggers PostAccount -> Release, yielding exactly one reservation
// in released state with no extra row.
func TestPipelineRunBudgetRelease(t *testing.T) {
	pool, cleanup := startTestPostgres(t)
	if pool == nil {
		return // skipped
	}
	defer cleanup()

	bgtSvc := budget.NewPgService(pool)

	adapter := &errorOnSecondReadAdapter{name: "anthropic"}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"anthropic-prod": {Wire: "anthropic", Vendor: "anthropic", URL: "https://api.anthropic.com"},
		},
		Pools: map[string]config.Pool{
			"standard": {
				Members:     []config.PoolMember{{EndpointID: "anthropic-prod", Model: "claude-sonnet-4-6", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, nil)

	pol, _ := policy.NewEngine(&config.PolicyConfig{
		Version: "1.0",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "route", ModelPool: "standard"},
		},
	})

	pricing := &config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor:                      "anthropic",
				Model:                       "claude-sonnet-4-6",
				InputPricePer1KTokens:       3.0,
				OutputPricePer1KTokens:      15.0,
				CacheReadPricePer1KTokens:   float64Ptr(0.30),
				CacheCreatePricePer1KTokens: float64Ptr(3.0),
				Capabilities:                config.ModelCapabilities{Tools: true, CacheControl: true},
			},
		},
	}

	calc := cost.NewCalculator(pricing)
	cfg := &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"anthropic-prod": {ResolvedKey: "test-key"},
		},
	}

	// Use an audit.Writer to suppress nil panics during event logging.
	egress := NewEgressPipeline(map[string]provider.Adapter{"anthropic": adapter}, calc, audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), calc, cfg)
	pipeline.PgBudgetSvc = bgtSvc

	traceID := "00000000-0000-0000-0000-000000000002"
	req := &ForwardRequest{
		Envelope: []byte(`{"trace_id":"` + traceID + `"}`),
		Wire: WirePayload{
			Protocol: "anthropic_messages",
			Stream:   true,
			Body:     []byte(`{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`),
		},
	}

	result, err := pipeline.Run(context.Background(), req, traceID)
	if err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}

	// Drain the stream. The upstream body will error on the second read,
	// triggering the stream_interrupted path in AccountedStream, which
	// calls PostAccount -> Release.
	_, _ = io.ReadAll(result.Body)
	_ = result.Body.Close()

	// Verify exactly one budget_reservation row exists for this trace.
	rows := queryBudgetReservations(t, pool, traceID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 budget reservation, got %d", len(rows))
	}
	if rows[0].State != "released" {
		t.Errorf("reservation state = %q, want released", rows[0].State)
	}
}
