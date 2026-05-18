package routing

import (
	"strings"
	"testing"

	"agentgate/internal/gw/config"
	"agentgate/internal/gw/policy"
)

func testRegistry() map[string]config.ProviderEndpoint {
	return map[string]config.ProviderEndpoint{
		"ep1": {Wire: "anthropic", Vendor: "anthropic", URL: "https://api.anthropic.com", DataResidency: "us", TrustTier: "vendor", Supports: config.EndpointSupports{Streaming: true}},
		"ep2": {Wire: "openai", Vendor: "openai", URL: "https://api.openai.com/v1", DataResidency: "us", TrustTier: "vendor", Supports: config.EndpointSupports{Streaming: true}},
		"ep3": {Wire: "openai_compat", Vendor: "ollama-local", URL: "https://ollama.internal:11434/v1", DataResidency: "on_prem", TrustTier: "private", Supports: config.EndpointSupports{Streaming: true}},
	}
}

func testPools() map[string]config.Pool {
	return map[string]config.Pool{
		"cheap":    {Members: []config.PoolMember{{EndpointID: "ep3", Model: "qwen", Weight: 100}}, FallbackPool: "standard", MaxAttempts: 2, TimeoutMs: 60000},
		"standard": {Members: []config.PoolMember{{EndpointID: "ep1", Model: "claude-sonnet", Weight: 60}, {EndpointID: "ep2", Model: "gpt-4o", Weight: 40}}, FallbackPool: "strong", MaxAttempts: 3, TimeoutMs: 90000},
		"strong":   {Members: []config.PoolMember{{EndpointID: "ep1", Model: "claude-opus", Weight: 100}}, FallbackPool: "", MaxAttempts: 2, TimeoutMs: 180000},
	}
}

func testPricing() *config.PricingConfig {
	return &config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "anthropic", Model: "claude-sonnet", Capabilities: config.ModelCapabilities{Tools: true, CacheControl: true, ExtendedThinking: true, Vision: true}},
			{Vendor: "anthropic", Model: "claude-opus", Capabilities: config.ModelCapabilities{Tools: true, CacheControl: true, ExtendedThinking: true, Vision: true}},
			{Vendor: "openai", Model: "gpt-4o", Capabilities: config.ModelCapabilities{Tools: true, CacheControl: false, ExtendedThinking: false, Vision: false}},
			{Vendor: "ollama-local", Model: "qwen", Capabilities: config.ModelCapabilities{Tools: false, CacheControl: false, ExtendedThinking: false, Vision: false}},
		},
	}
}

func testSelector() *Selector {
	return NewSelector(&config.PoolsConfig{
		ProviderEndpoints: testRegistry(),
		Pools:             testPools(),
	}, testPricing())
}

func TestBuildChainSuccess(t *testing.T) {
	sel := testSelector()
	dec := &policy.Decision{PrimaryAction: policy.ActionRoute}

	chain, err := sel.BuildChain("standard", dec, "trace-001", 1)
	if err != nil {
		t.Fatalf("BuildChain: %v", err)
	}
	if len(chain) == 0 {
		t.Fatal("expected non-empty chain")
	}
	if len(chain) != 2 {
		t.Errorf("expected 2 members, got %d", len(chain))
	}
}

func TestBuildChainDeterministic(t *testing.T) {
	sel := testSelector()
	dec := &policy.Decision{PrimaryAction: policy.ActionRoute}

	chain1, _ := sel.BuildChain("standard", dec, "trace-001", 1)
	chain2, _ := sel.BuildChain("standard", dec, "trace-001", 1)

	if len(chain1) != len(chain2) {
		t.Fatal("chains should be same length (deterministic seed)")
	}
	for i := range chain1 {
		if chain1[i].EndpointID != chain2[i].EndpointID {
			t.Errorf("position %d: first=%s second=%s — deterministic seed should produce same order", i, chain1[i].EndpointID, chain2[i].EndpointID)
		}
	}
}

func TestBuildChainErrNoCandidate(t *testing.T) {
	sel := testSelector()
	dec := &policy.Decision{
		PrimaryAction:        policy.ActionRoute,
		RequiredTrustTier:    "private",
		RequiredDataResidency: []string{"eu"},
	}

	_, err := sel.BuildChain("cheap", dec, "trace-001", 1)
	if err == nil {
		t.Fatal("expected ErrNoCandidate for impossible constraints")
	}
	if _, ok := err.(*ErrNoCandidate); !ok {
		t.Errorf("expected ErrNoCandidate, got %T: %v", err, err)
	}
}

func TestBuildChainConstraintFilter(t *testing.T) {
	sel := testSelector()
	dec := &policy.Decision{
		PrimaryAction:     policy.ActionRoute,
		RequiredTrustTier: "private",
	}

	// standard pool has ep1(vendor)+ep2(vendor) → 0 private candidates.
	_, err := sel.BuildChain("standard", dec, "trace-001", 1)
	if err == nil {
		t.Fatal("expected ErrNoCandidate when no private endpoint in standard pool")
	}

	// cheap pool has ep3(private) → should succeed.
	chain, err := sel.BuildChain("cheap", dec, "trace-001", 1)
	if err != nil {
		t.Fatalf("cheap pool with private: %v", err)
	}
	if len(chain) != 1 || chain[0].EndpointID != "ep3" {
		t.Errorf("expected ep3 in chain, got %v", chain)
	}
}

func TestBuildChainReplayRoundTrip(t *testing.T) {
	sel := testSelector()
	dec := &policy.Decision{PrimaryAction: policy.ActionRoute}

	// BuildChain produces a deterministic chain.
	chain1, err := sel.BuildChain("standard", dec, "trace-roundtrip", 1)
	if err != nil {
		t.Fatalf("BuildChain: %v", err)
	}

	// Simulate storing the routing_event row with the 16-byte seed.
	storedSeed := SeedBytes("trace-roundtrip", "standard", 1)
	row := RoutingEventRow{
		TraceID:      "trace-roundtrip",
		AttemptNo:    1,
		PoolSelected: "standard",
		Seed:         storedSeed,
	}

	// Replay must produce the exact same chain.
	chain2, err := Replay(row, testRegistry(), testPools())
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}

	if len(chain1) != len(chain2) {
		t.Fatalf("round-trip: BuildChain=%d members, Replay=%d members", len(chain1), len(chain2))
	}
	for i := range chain1 {
		if chain1[i].EndpointID != chain2[i].EndpointID {
			t.Errorf("round-trip position %d: BuildChain=%s, Replay=%s", i, chain1[i].EndpointID, chain2[i].EndpointID)
		}
	}
}

func TestMemberWireAndVendorIDPopulated(t *testing.T) {
	sel := testSelector()
	dec := &policy.Decision{PrimaryAction: policy.ActionRoute}

	chain, err := sel.BuildChain("standard", dec, "trace-001", 1)
	if err != nil {
		t.Fatalf("BuildChain: %v", err)
	}
	for _, m := range chain {
		if m.Wire == "" {
			t.Errorf("member %s has empty Wire", m.EndpointID)
		}
		if m.VendorID == "" {
			t.Errorf("member %s has empty VendorID", m.EndpointID)
		}
	}
}

func TestBuildChainCapabilityFilterFromPricing(t *testing.T) {
	// Two endpoints both serving the same model "glm-4.7" via different vendors
	// with divergent cache_control capabilities.
	registry := map[string]config.ProviderEndpoint{
		"glm-bigmodel": {Wire: "anthropic_compat", Vendor: "bigmodel-direct", URL: "https://open.bigmodel.cn/api/anthropic", TrustTier: "vendor", Supports: config.EndpointSupports{Streaming: true}},
		"glm-bailian":  {Wire: "openai_compat", Vendor: "bailian", URL: "https://bailian.aliyun.com/api", TrustTier: "vendor", Supports: config.EndpointSupports{Streaming: true}},
	}
	pools := map[string]config.Pool{
		"cn-glm": {Members: []config.PoolMember{
			{EndpointID: "glm-bigmodel", Model: "glm-4.7", Weight: 50},
			{EndpointID: "glm-bailian", Model: "glm-4.7", Weight: 50},
		}, MaxAttempts: 2, TimeoutMs: 90000},
	}
	pricing := &config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "bigmodel-direct", Model: "glm-4.7", Capabilities: config.ModelCapabilities{CacheControl: false}},
			{Vendor: "bailian", Model: "glm-4.7", Capabilities: config.ModelCapabilities{CacheControl: true}},
		},
	}
	sel := NewSelector(&config.PoolsConfig{ProviderEndpoints: registry, Pools: pools}, pricing)

	// With cache_control required, only Bailian should pass.
	dec := &policy.Decision{
		PrimaryAction:       policy.ActionRoute,
		RequiredCapabilities: []string{"cache_control"},
	}
	chain, err := sel.BuildChain("cn-glm", dec, "trace-cc", 1)
	if err != nil {
		t.Fatalf("BuildChain with cache_control: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("expected 1 candidate with cache_control, got %d", len(chain))
	}
	if chain[0].EndpointID != "glm-bailian" {
		t.Errorf("expected Bailian endpoint (cache_control:true), got %s", chain[0].EndpointID)
	}

	// Without constraints, both should appear.
	dec2 := &policy.Decision{PrimaryAction: policy.ActionRoute}
	chain2, err := sel.BuildChain("cn-glm", dec2, "trace-no-cc", 1)
	if err != nil {
		t.Fatalf("BuildChain without constraints: %v", err)
	}
	if len(chain2) != 2 {
		t.Errorf("expected 2 candidates without constraints, got %d", len(chain2))
	}
}

func TestMemberSupportsFailClosedOnMissingPricing(t *testing.T) {
	// Model in pool has no pricing row → all non-streaming caps fail closed (D6).
	pricing := &config.PricingConfig{
		Models: []config.ModelPricing{}, // empty pricing
	}
	m := Member{VendorID: "unknown", Model: "no-pricing", Streaming: true}

	if memberSupports(m, "streaming", pricing) != true {
		t.Error("streaming should still pass (endpoint-level, not pricing)")
	}
	if memberSupports(m, "cache_control", pricing) != false {
		t.Error("cache_control should fail closed on missing pricing row")
	}
	if memberSupports(m, "tools", pricing) != false {
		t.Error("tools should fail closed on missing pricing row")
	}
	if memberSupports(m, "extended_thinking", pricing) != false {
		t.Error("extended_thinking should fail closed on missing pricing row")
	}
	if memberSupports(m, "vision", pricing) != false {
		t.Error("vision should fail closed on missing pricing row")
	}
}

func TestValidatePoolAdaptersAllMatch(t *testing.T) {
	sel := testSelector()
	adapterNames := []string{"anthropic", "openai", "openai_compat"}
	if err := sel.ValidatePoolAdapters(adapterNames); err != nil {
		t.Fatalf("expected nil for fully covered adapters, got: %v", err)
	}
}

func TestValidatePoolAdaptersMissingWire(t *testing.T) {
	registry := map[string]config.ProviderEndpoint{
		"ollama-cluster": {Wire: "openai_compat", Vendor: "ollama-local", URL: "https://ollama.internal:11434/v1"},
	}
	pools := map[string]config.Pool{
		"test-pool": {
			Members:     []config.PoolMember{{EndpointID: "ollama-cluster", Model: "qwen2.5-coder:7b", Weight: 1}},
			MaxAttempts: 1,
		},
	}
	sel := NewSelector(&config.PoolsConfig{ProviderEndpoints: registry, Pools: pools}, nil)

	err := sel.ValidatePoolAdapters([]string{"anthropic"})
	if err == nil {
		t.Fatal("expected error for missing wire, got nil")
	}
	if !strings.Contains(err.Error(), "no registered adapter") {
		t.Errorf("expected 'no registered adapter' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "openai_compat") {
		t.Errorf("expected error to mention wire 'openai_compat', got: %v", err)
	}
	if !strings.Contains(err.Error(), "ollama-cluster") {
		t.Errorf("expected error to mention endpoint 'ollama-cluster', got: %v", err)
	}
}

func TestValidatePoolAdaptersMultipleMismatches(t *testing.T) {
	registry := map[string]config.ProviderEndpoint{
		"ep-a": {Wire: "wire_x", Vendor: "vx", URL: "https://x.example.com"},
		"ep-b": {Wire: "wire_y", Vendor: "vy", URL: "https://y.example.com"},
		"ep-c": {Wire: "anthropic", Vendor: "anthropic", URL: "https://api.anthropic.com"},
	}
	pools := map[string]config.Pool{
		"pool-1": {
			Members:     []config.PoolMember{{EndpointID: "ep-a", Model: "m1", Weight: 1}},
			MaxAttempts: 1,
		},
		"pool-2": {
			Members:     []config.PoolMember{{EndpointID: "ep-b", Model: "m2", Weight: 1}, {EndpointID: "ep-c", Model: "m3", Weight: 1}},
			MaxAttempts: 1,
		},
	}
	sel := NewSelector(&config.PoolsConfig{ProviderEndpoints: registry, Pools: pools}, nil)

	err := sel.ValidatePoolAdapters([]string{"anthropic"})
	if err == nil {
		t.Fatal("expected error for multiple mismatches, got nil")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "wire_x") {
		t.Errorf("expected error to mention wire_x, got: %v", err)
	}
	if !strings.Contains(errStr, "wire_y") {
		t.Errorf("expected error to mention wire_y, got: %v", err)
	}
	if strings.Contains(errStr, "ep-c") {
		t.Errorf("ep-c (anthropic) should not appear in error since it has a registered adapter")
	}
}

func TestValidatePoolAdaptersEmptyPools(t *testing.T) {
	sel := NewSelector(&config.PoolsConfig{ProviderEndpoints: map[string]config.ProviderEndpoint{}, Pools: map[string]config.Pool{}}, nil)
	if err := sel.ValidatePoolAdapters([]string{"anthropic"}); err != nil {
		t.Fatalf("expected nil for empty pools, got: %v", err)
	}
}

func TestSetPricingHotReload(t *testing.T) {
	// Simulate hot-reload: swap pricing, verify capability checks use new data.
	registry := map[string]config.ProviderEndpoint{
		"ep1": {Wire: "openai_compat", Vendor: "test-vendor", URL: "https://x.example.com", TrustTier: "vendor", Supports: config.EndpointSupports{Streaming: true}},
	}
	pools := map[string]config.Pool{
		"p1": {Members: []config.PoolMember{{EndpointID: "ep1", Model: "test-model", Weight: 100}}, MaxAttempts: 1, TimeoutMs: 60000},
	}
	// Initial pricing: cache_control = false.
	pricing1 := &config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "test-vendor", Model: "test-model", Capabilities: config.ModelCapabilities{CacheControl: false}},
		},
	}
	sel := NewSelector(&config.PoolsConfig{ProviderEndpoints: registry, Pools: pools}, pricing1)

	dec := &policy.Decision{PrimaryAction: policy.ActionRoute, RequiredCapabilities: []string{"cache_control"}}
	_, err := sel.BuildChain("p1", dec, "trace-1", 1)
	if err == nil {
		t.Fatal("expected ErrNoCandidate when cache_control is false")
	}

	// Hot-reload: swap to pricing with cache_control = true.
	pricing2 := &config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "test-vendor", Model: "test-model", Capabilities: config.ModelCapabilities{CacheControl: true}},
		},
	}
	sel.SetPricing(pricing2)

	chain, err := sel.BuildChain("p1", dec, "trace-2", 1)
	if err != nil {
		t.Fatalf("after SetPricing with cache_control=true: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("expected 1 candidate after pricing hot-reload, got %d", len(chain))
	}
}

func TestMemberSupportsNilPricingFailClosed(t *testing.T) {
	// nil PricingConfig must fail closed on non-streaming caps, not panic (D6).
	m := Member{VendorID: "v", Model: "m", Streaming: true}

	if memberSupports(m, "streaming", nil) != true {
		t.Error("streaming should pass even with nil pricing")
	}
	if memberSupports(m, "cache_control", nil) != false {
		t.Error("cache_control should fail closed with nil pricing")
	}
	if memberSupports(m, "tools", nil) != false {
		t.Error("tools should fail closed with nil pricing")
	}
	if memberSupports(m, "extended_thinking", nil) != false {
		t.Error("extended_thinking should fail closed with nil pricing")
	}
	if memberSupports(m, "vision", nil) != false {
		t.Error("vision should fail closed with nil pricing")
	}
}

func TestSeedBytes(t *testing.T) {
	s1 := SeedBytes("trace-1", "standard", 1)
	s2 := SeedBytes("trace-1", "standard", 1)
	s3 := SeedBytes("trace-2", "standard", 1)

	if string(s1) != string(s2) {
		t.Error("same inputs should produce same seed")
	}
	if string(s1) == string(s3) {
		t.Error("different trace_id should produce different seed")
	}
	if len(s1) != 16 {
		t.Errorf("expected 16-byte seed, got %d", len(s1))
	}
}
