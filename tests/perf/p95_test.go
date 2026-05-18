// Package perf provides the P0 performance SLO test: N=200 serial requests,
// GW internal latency p95 < 150ms (§20.5, pre-exec rev #3).
package perf

import (
	"sort"
	"testing"
	"time"

	"agentgate/internal/gw/edge"
	"agentgate/internal/gw/routing"
	"agentgate/internal/gw/config"
	"agentgate/internal/gw/policy"
)

// TestGWInternalP95 asserts GW internal timing p95 < 150ms.
// Uses zero-delay mock routing + decision to isolate GW code paths.
func TestGWInternalP95(t *testing.T) {
	const N = 200
	const p95Target = 150 * time.Millisecond

	// Setup: minimal GW components (no network, no Postgres).
	poolsCfg := &config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"ep1": {Wire: "anthropic", Vendor: "anthropic", URL: "http://localhost", DataResidency: "us", TrustTier: "vendor", Supports: config.EndpointSupports{Streaming: true}},
			"ep2": {Wire: "openai", Vendor: "openai", URL: "http://localhost", DataResidency: "us", TrustTier: "vendor", Supports: config.EndpointSupports{Streaming: true}},
		},
		Pools: map[string]config.Pool{
			"standard": {
				Members: []config.PoolMember{
					{EndpointID: "ep1", Model: "claude-sonnet-4-6", Weight: 60},
					{EndpointID: "ep2", Model: "gpt-4o", Weight: 40},
				},
				MaxAttempts: 1,
				TimeoutMs:   90000,
			},
		},
	}
	sel := routing.NewSelector(poolsCfg, nil)

	// Policy engine (unused in routing path, but verified for construction overhead).
	policyCfg := &config.PolicyConfig{
		Version: "1",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "route", ModelPool: "standard", Reasons: []string{"default"}},
		},
	}
	eng, err := policy.NewEngine(policyCfg)
	if err != nil {
		t.Fatalf("policy engine: %v", err)
	}

	var latencies []time.Duration

	for i := 0; i < N; i++ {
		traceID := edge.MintTraceID()
		start := time.Now()

		// Route
		chain, err := sel.BuildChain("standard", nil, traceID, 1)
		if err != nil {
			t.Fatalf("request %d: BuildChain: %v", i, err)
		}

		// Seed
		seed := routing.SeedBytes(traceID, "standard", 1)

		// Policy decision
		dec, err := eng.Decide(policy.EvalInput{
			ServerClass: map[string]any{"task_type": "code_edit"},
		})
		if err != nil {
			t.Fatalf("request %d: Decide: %v", i, err)
		}

		elapsed := time.Since(start)
		latencies = append(latencies, elapsed)

		// Prevent compiler optimizations from eliminating the work.
		_ = chain
		_ = seed
		_ = dec
	}

	// Compute percentiles.
	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	p50 := latencies[N*50/100]
	p95 := latencies[N*95/100]
	p99 := latencies[N*99/100]

	t.Logf("perf results (N=%d): p50=%v p95=%v p99=%v", N, p50, p95, p99)

	if p95 > p95Target {
		// CI artifact: log all latencies for debugging.
		for i, l := range latencies {
			if i%20 == 0 {
				t.Logf("  [%03d] %v", i, l)
			}
		}
		t.Errorf("p95=%v exceeds target %v (p50=%v, p99=%v)", p95, p95Target, p50, p99)
	}
}

// TestChaCha8RNGOverhead measures the ChaCha8 RNG construction overhead.
func TestChaCha8RNGOverhead(t *testing.T) {
	const N = 500
	seed := routing.SeedBytes("trace-perf", "standard", 1)
	var seed32 [32]byte
	copy(seed32[:], seed)
	for i := 16; i < 32; i++ {
		seed32[i] = seed32[i-16]
	}

	start := time.Now()
	for i := 0; i < N; i++ {
		rng := routing.NewChaCha8RNG(seed32)
		for j := 0; j < 100; j++ {
			_ = rng.IntN(100)
		}
	}
	elapsed := time.Since(start)
	avgPerRNG := elapsed / N
	t.Logf("ChaCha8 RNG overhead: %v per RNG (100 calls each, N=%d)", avgPerRNG, N)
	// Each RNG construction + 100 IntN calls should be well under 1ms.
	if avgPerRNG > 10*time.Millisecond {
		t.Errorf("ChaCha8 RNG too slow: %v per N", avgPerRNG)
	}
}

// TestTraceIDMintingOverhead measures UUIDv7 generation speed.
func TestTraceIDMintingOverhead(t *testing.T) {
	const N = 1000
	start := time.Now()
	for i := 0; i < N; i++ {
		_ = edge.MintTraceID()
	}
	elapsed := time.Since(start)
	avg := elapsed / N
	t.Logf("UUIDv7 mint: %v per ID (N=%d)", avg, N)
	if avg > time.Millisecond {
		t.Errorf("UUIDv7 mint too slow: %v per ID", avg)
	}
}

// TestCELDecisionOverhead measures policy decision latency.
func TestCELDecisionOverhead(t *testing.T) {
	cfg := &config.PolicyConfig{
		Version: "1",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "route", ModelPool: "standard", Reasons: []string{"default"}},
		},
		Rules: []config.PolicyRule{
			{ID: "P-ROUTE-001", Priority: 500, When: `server_class.task_type in ["code_edit","simple_edit"]`, Action: "route", ModelPool: "standard"},
			{ID: "P-ROUTE-002", Priority: 500, When: `server_class.task_type in ["summary"]`, Action: "route", ModelPool: "cheap"},
			{ID: "P-ROUTE-003", Priority: 500, When: `server_class.task_type in ["debug"]`, Action: "route", ModelPool: "strong"},
		},
	}
	eng, err := policy.NewEngine(cfg)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	const N = 200
	input := policy.EvalInput{
		ServerClass: map[string]any{"task_type": "code_edit"},
	}
	start := time.Now()
	for i := 0; i < N; i++ {
		_, err := eng.Decide(input)
		if err != nil {
			t.Fatalf("Decide: %v", err)
		}
	}
	elapsed := time.Since(start)
	avg := elapsed / N
	t.Logf("CEL decision latency: %v avg over %d evaluations", avg, N)
	if avg > 10*time.Millisecond {
		t.Errorf("CEL decision too slow: %v avg", avg)
	}
}

