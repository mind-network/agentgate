package cost

import (
	"testing"

	"agentgate/internal/gw/config"
	"agentgate/internal/shared/ir"
)

func float64Ptr(v float64) *float64 {
	return &v
}

func TestCalculateProviderUsage(t *testing.T) {
	calc := NewCalculator(&config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "anthropic", Model: "claude-sonnet-4-6", Currency: "USD", InputPricePer1KTokens: 3.0, OutputPricePer1KTokens: 15.0},
		},
	})

	totalCents, inputCost, outputCost, cacheReadCost, cacheCreateCost, currency, source := calc.Calculate("anthropic", "claude-sonnet-4-6", 1000, 500, 0, 0)
	if source != "provider_usage" {
		t.Errorf("expected provider_usage, got %s", source)
	}
	if currency != "USD" {
		t.Errorf("expected USD, got %s", currency)
	}
	if totalCents != 10 {
		t.Errorf("expected total 10 cents, got %d", totalCents)
	}
	if inputCost != 3 {
		t.Errorf("expected input cost 3 cents, got %d", inputCost)
	}
	if outputCost != 7 {
		t.Errorf("expected output cost 7 cents, got %d", outputCost)
	}
	if cacheReadCost != 0 {
		t.Errorf("expected cache_read cost 0, got %d", cacheReadCost)
	}
	if cacheCreateCost != 0 {
		t.Errorf("expected cache_create cost 0, got %d", cacheCreateCost)
	}
}

func TestCalculateSmallTokensTotalCostRegression(t *testing.T) {
	// Regression: prior aggregate formula floored once at the end, so
	// 256 input at 3.0 + 128 output at 15.0 = floor(2688/1000) = 2.
	// Per-bucket floor gives input=0, output=1, but total must be 2.
	calc := NewCalculator(&config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "anthropic", Model: "claude-sonnet-4-6", Currency: "USD", InputPricePer1KTokens: 3.0, OutputPricePer1KTokens: 15.0},
		},
	})

	total, inputCost, outputCost, cacheReadCost, cacheCreateCost, currency, source := calc.Calculate("anthropic", "claude-sonnet-4-6", 256, 128, 0, 0)
	if source != "provider_usage" {
		t.Errorf("expected provider_usage, got %s", source)
	}
	if currency != "USD" {
		t.Errorf("expected USD, got %s", currency)
	}
	if total != 2 {
		t.Errorf("expected total 2 cents (float sum floor), got %d", total)
	}
	// Per-bucket values are individually floored.
	if inputCost != 0 {
		t.Errorf("expected input cost 0 (floor of 0.768), got %d", inputCost)
	}
	if outputCost != 1 {
		t.Errorf("expected output cost 1 (floor of 1.92), got %d", outputCost)
	}
	if cacheReadCost != 0 {
		t.Errorf("expected cache_read cost 0, got %d", cacheReadCost)
	}
	if cacheCreateCost != 0 {
		t.Errorf("expected cache_create cost 0, got %d", cacheCreateCost)
	}
}

func TestCalculateWithCNY(t *testing.T) {
	calc := NewCalculator(&config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "deepseek-direct", Model: "deepseek-v4-pro", Currency: "CNY", InputPricePer1KTokens: 2.0, OutputPricePer1KTokens: 8.0},
		},
	})

	totalCents, _, _, _, _, currency, source := calc.Calculate("deepseek-direct", "deepseek-v4-pro", 1000, 500, 0, 0)
	if source != "provider_usage" {
		t.Errorf("expected provider_usage, got %s", source)
	}
	if currency != "CNY" {
		t.Errorf("expected CNY, got %s", currency)
	}
	if totalCents != 6 {
		t.Errorf("expected 6 fen, got %d", totalCents)
	}
}

func TestCalculateEstimated(t *testing.T) {
	calc := NewCalculator(&config.PricingConfig{
		Models: []config.ModelPricing{},
	})

	totalCents, _, _, _, _, currency, source := calc.Calculate("unknown", "unknown-model", 1000, 500, 0, 0)
	if source != "estimated" {
		t.Errorf("expected estimated, got %s", source)
	}
	if currency != "USD" {
		t.Errorf("expected default USD, got %s", currency)
	}
	if totalCents != 0 {
		t.Errorf("expected 0 cost for estimated, got %d", totalCents)
	}
}

func TestCalculateDefaultCurrency(t *testing.T) {
	cfg := &config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "test", Model: "test-model", InputPricePer1KTokens: 1.0, OutputPricePer1KTokens: 2.0},
		},
	}
	cfg.DefaultCurrencies()

	calc := NewCalculator(cfg)
	_, _, _, _, _, currency, _ := calc.Calculate("test", "test-model", 0, 0, 0, 0)
	if currency != "USD" {
		t.Errorf("expected default USD, got %s", currency)
	}
}

func TestCalculateWithCacheTokens(t *testing.T) {
	calc := NewCalculator(&config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor:                      "anthropic",
				Model:                       "claude-sonnet-4-6",
				Currency:                    "USD",
				InputPricePer1KTokens:       3.0,
				OutputPricePer1KTokens:      15.0,
				CacheReadPricePer1KTokens:   float64Ptr(0.30),
				CacheCreatePricePer1KTokens: float64Ptr(3.75),
				Capabilities:                config.ModelCapabilities{CacheControl: true},
			},
		},
	})

	// 1000 input, 500 output, 2000 cache read, 300 cache create
	total, inputCost, outputCost, cacheReadCost, cacheCreateCost, currency, source := calc.Calculate("anthropic", "claude-sonnet-4-6", 1000, 500, 2000, 300)
	if source != "provider_usage" {
		t.Errorf("expected provider_usage, got %s", source)
	}
	if currency != "USD" {
		t.Errorf("expected USD, got %s", currency)
	}
	// input: 1000 * 3.0 / 1000 = 3
	if inputCost != 3 {
		t.Errorf("expected input cost 3, got %d", inputCost)
	}
	// output: 500 * 15.0 / 1000 = 7
	if outputCost != 7 {
		t.Errorf("expected output cost 7, got %d", outputCost)
	}
	// cache_read: 2000 * 0.30 / 1000 = 0 (integer division)
	if cacheReadCost != 0 {
		t.Errorf("expected cache_read cost 0, got %d", cacheReadCost)
	}
	// cache_create: 300 * 3.75 / 1000 = 1
	if cacheCreateCost != 1 {
		t.Errorf("expected cache_create cost 1, got %d", cacheCreateCost)
	}
	// total: int(3.0 + 7.5 + 0.6 + 1.125) = 12
	if total != 12 {
		t.Errorf("expected total 12, got %d", total)
	}
}

func TestCalculateCacheTokensNoCachePrice(t *testing.T) {
	// Model with cache_control=true but no explicit cache price fields.
	calc := NewCalculator(&config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor:                 "anthropic",
				Model:                  "claude-sonnet-4-6",
				Currency:               "USD",
				InputPricePer1KTokens:  3.0,
				OutputPricePer1KTokens: 15.0,
				// CacheReadPricePer1KTokens and CacheCreatePricePer1KTokens are nil
				Capabilities: config.ModelCapabilities{CacheControl: true},
			},
		},
	})

	// The config wouldn't pass validation at load time, but at runtime
	// the calculator should fall back to input price and mark estimated.
	total, _, _, cacheReadCost, cacheCreateCost, _, source := calc.Calculate("anthropic", "claude-sonnet-4-6", 1000, 500, 1000, 500)
	if source != "estimated" {
		t.Errorf("expected estimated due to missing cache price, got %s", source)
	}
	// Fallback uses input price for both: 1000*3.0/1000 = 3, 500*3.0/1000 = 1
	if cacheReadCost != 3 {
		t.Errorf("expected cache_read fallback cost 3, got %d", cacheReadCost)
	}
	if cacheCreateCost != 1 {
		t.Errorf("expected cache_create fallback cost 1, got %d", cacheCreateCost)
	}
	if total != 15 { // int(3.0+7.5+3.0+1.5) = 15
		t.Errorf("expected total 15 (float sum 3.0+7.5+3.0+1.5), got %d", total)
	}
}

func TestCalculateCacheTokensOnlyInputFallback(t *testing.T) {
	// Only cache_read price is set; cache_create should fall back to input price.
	calc := NewCalculator(&config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor:                      "anthropic",
				Model:                       "claude-sonnet-4-6",
				Currency:                    "USD",
				InputPricePer1KTokens:       3.0,
				OutputPricePer1KTokens:      15.0,
				CacheReadPricePer1KTokens:   float64Ptr(0.30),
				CacheCreatePricePer1KTokens: nil,
			},
		},
	})

	total, _, _, cacheReadCost, cacheCreateCost, _, source := calc.Calculate("anthropic", "claude-sonnet-4-6", 1000, 500, 1000, 300)
	if source != "estimated" {
		t.Errorf("expected estimated due to missing cache_create price, got %s", source)
	}
	// cache_read uses explicit price: 1000*0.30/1000 = 0
	if cacheReadCost != 0 {
		t.Errorf("expected cache_read cost 0, got %d", cacheReadCost)
	}
	// cache_create falls back to input: 300*3.0/1000 = 0
	if cacheCreateCost != 0 {
		t.Errorf("expected cache_create fallback cost 0, got %d", cacheCreateCost)
	}
	_ = total
}

func TestCalculateZeroCostLocalModelNoCache(t *testing.T) {
	calc := NewCalculator(&config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor:                 "ollama-local",
				Model:                  "qwen2.5-coder:7b",
				Currency:               "USD",
				InputPricePer1KTokens:  0,
				OutputPricePer1KTokens: 0,
				// No cache prices, no cache_control capability
			},
		},
	})

	total, inputCost, outputCost, cacheReadCost, cacheCreateCost, currency, source := calc.Calculate("ollama-local", "qwen2.5-coder:7b", 1000, 500, 0, 0)
	if source != "provider_usage" {
		t.Errorf("expected provider_usage for zero-cost model, got %s", source)
	}
	if currency != "USD" {
		t.Errorf("expected USD, got %s", currency)
	}
	if total != 0 {
		t.Errorf("expected total 0, got %d", total)
	}
	if inputCost != 0 || outputCost != 0 || cacheReadCost != 0 || cacheCreateCost != 0 {
		t.Errorf("expected all costs 0, got input=%d output=%d cache_read=%d cache_create=%d", inputCost, outputCost, cacheReadCost, cacheCreateCost)
	}
}

func TestBuildEvent(t *testing.T) {
	calc := NewCalculator(&config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "anthropic", Model: "claude-sonnet-4-6", Currency: "USD", InputPricePer1KTokens: 3.0, OutputPricePer1KTokens: 15.0},
		},
	})

	usage := &ir.Usage{Input: 1000, Output: 500}
	ev := BuildEvent(usage, "anthropic", "anthropic", "ep1", "claude-sonnet-4-6", "standard", false, calc)

	if ev.Wire != "anthropic" {
		t.Errorf("wire = %q", ev.Wire)
	}
	if ev.VendorID != "anthropic" {
		t.Errorf("vendor_id = %q", ev.VendorID)
	}
	if ev.CostSource != "provider_usage" {
		t.Errorf("cost_source = %q", ev.CostSource)
	}
	if ev.Currency != "USD" {
		t.Errorf("currency = %q, want USD", ev.Currency)
	}
	if ev.CostCents != 10 {
		t.Errorf("cost_cents = %d, want 10", ev.CostCents)
	}
	if ev.CacheReadTokens != 0 {
		t.Errorf("cache_read_tokens = %d, want 0", ev.CacheReadTokens)
	}
	if ev.InputCostCents != 3 {
		t.Errorf("input_cost_cents = %d, want 3", ev.InputCostCents)
	}
	if ev.OutputCostCents != 7 {
		t.Errorf("output_cost_cents = %d, want 7", ev.OutputCostCents)
	}
	if ev.CacheReadCostCents != 0 {
		t.Errorf("cache_read_cost_cents = %d, want 0", ev.CacheReadCostCents)
	}
	if ev.CacheCreateCostCents != 0 {
		t.Errorf("cache_create_cost_cents = %d, want 0", ev.CacheCreateCostCents)
	}
}

func TestBuildEventCNY(t *testing.T) {
	calc := NewCalculator(&config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "deepseek-direct", Model: "deepseek-v4-pro", Currency: "CNY", InputPricePer1KTokens: 2.0, OutputPricePer1KTokens: 8.0},
		},
	})

	usage := &ir.Usage{Input: 1000, Output: 500}
	ev := BuildEvent(usage, "anthropic_compat", "deepseek-direct", "deepseek-anthropic", "deepseek-v4-pro", "cn-default", false, calc)

	if ev.Currency != "CNY" {
		t.Errorf("currency = %q, want CNY", ev.Currency)
	}
	if ev.CostCents != 6 {
		t.Errorf("cost_cents = %d, want 6", ev.CostCents)
	}
	if ev.Wire != "anthropic_compat" {
		t.Errorf("wire = %q, want anthropic_compat", ev.Wire)
	}
	if ev.VendorID != "deepseek-direct" {
		t.Errorf("vendor_id = %q, want deepseek-direct", ev.VendorID)
	}
}

func TestBuildEventWithCache(t *testing.T) {
	calc := NewCalculator(&config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor:                      "anthropic",
				Model:                       "claude-sonnet-4-6",
				Currency:                    "USD",
				InputPricePer1KTokens:       3.0,
				OutputPricePer1KTokens:      15.0,
				CacheReadPricePer1KTokens:   float64Ptr(0.30),
				CacheCreatePricePer1KTokens: float64Ptr(3.75),
				Capabilities:                config.ModelCapabilities{CacheControl: true},
			},
		},
	})

	usage := &ir.Usage{Input: 1000, Output: 500, CacheRead: 2000, CacheCreate: 300}
	ev := BuildEvent(usage, "anthropic", "anthropic", "ep1", "claude-sonnet-4-6", "standard", false, calc)

	if ev.CostCents != 12 {
		t.Errorf("cost_cents = %d, want 12", ev.CostCents)
	}
	if ev.CacheReadTokens != 2000 {
		t.Errorf("cache_read_tokens = %d, want 2000", ev.CacheReadTokens)
	}
	if ev.CacheCreateTokens != 300 {
		t.Errorf("cache_create_tokens = %d, want 300", ev.CacheCreateTokens)
	}
	if ev.InputCostCents != 3 {
		t.Errorf("input_cost_cents = %d, want 3", ev.InputCostCents)
	}
	if ev.OutputCostCents != 7 {
		t.Errorf("output_cost_cents = %d, want 7", ev.OutputCostCents)
	}
	if ev.CacheReadCostCents != 0 {
		t.Errorf("cache_read_cost_cents = %d, want 0", ev.CacheReadCostCents)
	}
	if ev.CacheCreateCostCents != 1 {
		t.Errorf("cache_create_cost_cents = %d, want 1", ev.CacheCreateCostCents)
	}
	if ev.CostSource != "provider_usage" {
		t.Errorf("cost_source = %q, want provider_usage", ev.CostSource)
	}
}

func TestBuildEventNilUsage(t *testing.T) {
	calc := NewCalculator(&config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "anthropic", Model: "claude-sonnet-4-6", Currency: "USD", InputPricePer1KTokens: 3.0, OutputPricePer1KTokens: 15.0},
		},
	})

	ev := BuildEvent(nil, "anthropic", "anthropic", "ep1", "claude-sonnet-4-6", "standard", false, calc)
	if ev.InputTokens != 0 || ev.OutputTokens != 0 || ev.CacheReadTokens != 0 || ev.CacheCreateTokens != 0 {
		t.Errorf("expected all zero tokens for nil usage, got input=%d output=%d cache_read=%d cache_create=%d", ev.InputTokens, ev.OutputTokens, ev.CacheReadTokens, ev.CacheCreateTokens)
	}
	if ev.CostCents != 0 {
		t.Errorf("expected 0 cost for nil usage, got %d", ev.CostCents)
	}
}
