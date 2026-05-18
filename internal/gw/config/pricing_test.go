package config

import (
	"testing"
)

func TestLoadPricingSuccess(t *testing.T) {
	path := writeTemp(t, "pricing-valid.yaml", validPricingYAML)
	cfg, err := LoadPricing(path)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if len(cfg.Models) != 6 {
		t.Errorf("expected 6 models, got %d", len(cfg.Models))
	}
}

func TestLoadPricingEmpty(t *testing.T) {
	path := writeTemp(t, "pricing-empty.yaml", "models: []\n")
	_, err := LoadPricing(path)
	if err == nil {
		t.Fatal("expected error for empty pricing, got nil")
	}
}

func TestLoadPricingNegativePrice(t *testing.T) {
	path := writeTemp(t, "pricing-neg.yaml", `
models:
  - vendor: test
    model: m1
    input_price_per_1k_tokens: -1.0
    output_price_per_1k_tokens: 0.0
`)
	_, err := LoadPricing(path)
	if err == nil {
		t.Fatal("expected error for negative price, got nil")
	}
}

func TestLoadPricingMissingVendor(t *testing.T) {
	path := writeTemp(t, "pricing-missing-vendor.yaml", `
models:
  - vendor: ""
    model: m1
    input_price_per_1k_tokens: 1.0
    output_price_per_1k_tokens: 1.0
`)
	_, err := LoadPricing(path)
	if err == nil {
		t.Fatal("expected error for missing vendor, got nil")
	}
}

func TestLoadPricingDuplicateVendorModel(t *testing.T) {
	path := writeTemp(t, "pricing-dup.yaml", `
models:
  - vendor: anthropic
    model: claude-sonnet-4-6
    input_price_per_1k_tokens: 3.00
    output_price_per_1k_tokens: 15.00
  - vendor: anthropic
    model: claude-sonnet-4-6
    input_price_per_1k_tokens: 5.00
    output_price_per_1k_tokens: 20.00
`)
	_, err := LoadPricing(path)
	if err == nil {
		t.Fatal("expected error for duplicate (vendor, model), got nil")
	}
	if err != nil && !contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate error, got: %v", err)
	}
}

func TestPricingLookup(t *testing.T) {
	cfg := &PricingConfig{
		Models: []ModelPricing{
			{Vendor: "anthropic", Model: "claude-sonnet-4-6", InputPricePer1KTokens: 3.00, OutputPricePer1KTokens: 15.00},
			{Vendor: "openai", Model: "gpt-4o", InputPricePer1KTokens: 2.50, OutputPricePer1KTokens: 10.00},
			{Vendor: "anthropic", Model: "claude-opus-4-7", InputPricePer1KTokens: 15.00, OutputPricePer1KTokens: 75.00},
		},
	}
	// Found.
	mp := cfg.Lookup("anthropic", "claude-opus-4-7")
	if mp == nil {
		t.Fatal("expected to find (anthropic, claude-opus-4-7)")
	}
	if mp.OutputPricePer1KTokens != 75.00 {
		t.Errorf("expected output price 75.00, got %.2f", mp.OutputPricePer1KTokens)
	}
	// Not found — wrong vendor.
	if mp := cfg.Lookup("openai", "claude-sonnet-4-6"); mp != nil {
		t.Errorf("expected nil for (openai, claude-sonnet-4-6), got %+v", mp)
	}
	// Not found — wrong model.
	if mp := cfg.Lookup("anthropic", "nonexistent"); mp != nil {
		t.Errorf("expected nil for (anthropic, nonexistent), got %+v", mp)
	}
}

func TestValidateEndpointsAgainstPricing(t *testing.T) {
	pricing := &PricingConfig{
		Models: []ModelPricing{
			{Vendor: "anthropic", Model: "claude-sonnet-4-6"},
			{Vendor: "openai", Model: "gpt-4o"},
		},
	}
	// Case 1: all endpoints have priced models → no error.
	pools := &PoolsConfig{
		ProviderEndpoints: map[string]ProviderEndpoint{
			"anthropic-prod": {Wire: "anthropic", Vendor: "anthropic"},
			"openai-prod":    {Wire: "openai", Vendor: "openai"},
		},
		Pools: map[string]Pool{
			"p1": {Members: []PoolMember{{EndpointID: "anthropic-prod", Model: "claude-sonnet-4-6", Weight: 100}}},
			"p2": {Members: []PoolMember{{EndpointID: "openai-prod", Model: "gpt-4o", Weight: 100}}},
		},
	}
	if err := ValidateEndpointsAgainstPricing(pools, pricing); err != nil {
		t.Errorf("expected success, got: %v", err)
	}

	// Case 2: endpoint vendor not in pricing at all → error.
	pools2 := &PoolsConfig{
		ProviderEndpoints: map[string]ProviderEndpoint{
			"deepseek-ep": {Wire: "anthropic_compat", Vendor: "deepseek-direct"},
		},
		Pools: map[string]Pool{
			"p1": {Members: []PoolMember{{EndpointID: "deepseek-ep", Model: "deepseek-v4-pro", Weight: 100}}},
		},
	}
	err := ValidateEndpointsAgainstPricing(pools2, pricing)
	if err == nil {
		t.Fatal("expected error for vendor with no pricing entries, got nil")
	}
	if !contains(err.Error(), "has no pricing entries") {
		t.Errorf("expected 'has no pricing entries' error, got: %v", err)
	}

	// Case 3: vendor exists in pricing but not for the pool member's model → error.
	pools3 := &PoolsConfig{
		ProviderEndpoints: map[string]ProviderEndpoint{
			"anthropic-prod": {Wire: "anthropic", Vendor: "anthropic"},
		},
		Pools: map[string]Pool{
			"p1": {Members: []PoolMember{{EndpointID: "anthropic-prod", Model: "claude-opus-4-7", Weight: 100}}},
		},
	}
	err = ValidateEndpointsAgainstPricing(pools3, pricing)
	if err == nil {
		t.Fatal("expected error for vendor with no pricing row for model, got nil")
	}
	if !contains(err.Error(), "has no pricing row for any model") {
		t.Errorf("expected 'no pricing row for any model' error, got: %v", err)
	}

	// Case 4: endpoint with no pool members → vacuously valid (skipped).
	pools4 := &PoolsConfig{
		ProviderEndpoints: map[string]ProviderEndpoint{
			"unused-ep": {Wire: "openai_compat", Vendor: "nonexistent-vendor"},
		},
		Pools: map[string]Pool{
			"p1": {Members: []PoolMember{{EndpointID: "other-ep", Model: "m", Weight: 100}}},
		},
	}
	if err := ValidateEndpointsAgainstPricing(pools4, pricing); err != nil {
		t.Errorf("unused endpoint should be skipped, got: %v", err)
	}
}

func TestPricingMissingCachePrice(t *testing.T) {
	// Cache-capable model must declare cache_read_price.
	t.Run("missing_cache_read_price", func(t *testing.T) {
		path := writeTemp(t, "pricing-no-cache-read.yaml", `
models:
  - vendor: anthropic
    model: claude-sonnet-4-6
    input_price_per_1k_tokens: 3.00
    output_price_per_1k_tokens: 15.00
    capabilities:
      cache_control: true
`)
		_, err := LoadPricing(path)
		if err == nil {
			t.Fatal("expected error for missing cache_read_price, got nil")
		}
	})

	// Cache-capable model must declare cache_create_price.
	t.Run("missing_cache_create_price", func(t *testing.T) {
		path := writeTemp(t, "pricing-no-cache-create.yaml", `
models:
  - vendor: anthropic
    model: claude-sonnet-4-6
    input_price_per_1k_tokens: 3.00
    output_price_per_1k_tokens: 15.00
    cache_read_price_per_1k_tokens: 0.30
    capabilities:
      cache_control: true
`)
		_, err := LoadPricing(path)
		if err == nil {
			t.Fatal("expected error for missing cache_create_price, got nil")
		}
	})
}

func TestPricingCachePriceNegative(t *testing.T) {
	t.Run("cache_control_true", func(t *testing.T) {
		path := writeTemp(t, "pricing-cache-neg.yaml", `
models:
  - vendor: anthropic
    model: claude-sonnet-4-6
    input_price_per_1k_tokens: 3.00
    output_price_per_1k_tokens: 15.00
    cache_read_price_per_1k_tokens: -1.00
    cache_create_price_per_1k_tokens: 3.75
    capabilities:
      cache_control: true
`)
		_, err := LoadPricing(path)
		if err == nil {
			t.Fatal("expected error for negative cache_read_price, got nil")
		}
	})

	// Regression: reject negative cache prices even when cache_control is false.
	t.Run("cache_control_false", func(t *testing.T) {
		path := writeTemp(t, "pricing-cache-neg-cache-control-false.yaml", `
models:
  - vendor: anthropic
    model: claude-sonnet-4-6
    input_price_per_1k_tokens: 3.00
    output_price_per_1k_tokens: 15.00
    cache_read_price_per_1k_tokens: -1.00
    cache_create_price_per_1k_tokens: 3.75
    capabilities:
      cache_control: false
`)
		_, err := LoadPricing(path)
		if err == nil {
			t.Fatal("expected error for negative cache_read_price (cache_control=false), got nil")
		}
	})
}

func TestPricingValidWithCachePrices(t *testing.T) {
	path := writeTemp(t, "pricing-valid-cache.yaml", `
models:
  - vendor: anthropic
    model: claude-sonnet-4-6
    input_price_per_1k_tokens: 3.00
    output_price_per_1k_tokens: 15.00
    cache_read_price_per_1k_tokens: 0.30
    cache_create_price_per_1k_tokens: 3.75
    capabilities:
      cache_control: true
  - vendor: openai
    model: gpt-4o
    input_price_per_1k_tokens: 2.50
    output_price_per_1k_tokens: 10.00
    capabilities:
      cache_control: false
  - vendor: ollama-local
    model: qwen2.5-coder:7b
    input_price_per_1k_tokens: 0
    output_price_per_1k_tokens: 0
`)
	cfg, err := LoadPricing(path)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if len(cfg.Models) != 3 {
		t.Errorf("expected 3 models, got %d", len(cfg.Models))
	}
}

func TestPricingLookupWithCache(t *testing.T) {
	cacheRead := 0.30
	cacheCreate := 3.75
	cfg := &PricingConfig{
		Models: []ModelPricing{
			{
				Vendor:                      "anthropic",
				Model:                       "claude-sonnet-4-6",
				InputPricePer1KTokens:       3.00,
				OutputPricePer1KTokens:      15.00,
				CacheReadPricePer1KTokens:   &cacheRead,
				CacheCreatePricePer1KTokens: &cacheCreate,
				Capabilities:                ModelCapabilities{CacheControl: true},
			},
		},
	}
	mp := cfg.Lookup("anthropic", "claude-sonnet-4-6")
	if mp == nil {
		t.Fatal("expected to find (anthropic, claude-sonnet-4-6)")
	}
	if mp.CacheReadPricePer1KTokens == nil {
		t.Fatal("expected cache_read price to be non-nil")
	}
	if *mp.CacheReadPricePer1KTokens != 0.30 {
		t.Errorf("expected cache_read price 0.30, got %.2f", *mp.CacheReadPricePer1KTokens)
	}
	if mp.CacheCreatePricePer1KTokens == nil {
		t.Fatal("expected cache_create price to be non-nil")
	}
	if *mp.CacheCreatePricePer1KTokens != 3.75 {
		t.Errorf("expected cache_create price 3.75, got %.2f", *mp.CacheCreatePricePer1KTokens)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

const validPricingYAML = `
models:
  - vendor: anthropic
    model: claude-opus-4-7
    input_price_per_1k_tokens: 15.00
    output_price_per_1k_tokens: 75.00
  - vendor: anthropic
    model: claude-sonnet-4-6
    input_price_per_1k_tokens: 3.00
    output_price_per_1k_tokens: 15.00
  - vendor: openai
    model: gpt-4o
    input_price_per_1k_tokens: 2.50
    output_price_per_1k_tokens: 10.00
  - vendor: openai
    model: o3
    input_price_per_1k_tokens: 10.00
    output_price_per_1k_tokens: 40.00
  - vendor: ollama-local
    model: qwen2.5-coder:7b
    input_price_per_1k_tokens: 0
    output_price_per_1k_tokens: 0
  - vendor: vllm-local
    model: llama3.1-405b
    input_price_per_1k_tokens: 0
    output_price_per_1k_tokens: 0
`
