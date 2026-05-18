// Package cost handles cost extraction, pricing lookup, and estimation.
package cost

import (
	"sync/atomic"

	"agentgate/internal/gw/config"
	"agentgate/internal/shared/ir"
)

// Event holds the computed cost data for one request.
type Event struct {
	Wire                 string `json:"wire"`
	VendorID             string `json:"vendor_id"`
	EndpointID           string `json:"endpoint_id"`
	Model                string `json:"model"`
	Pool                 string `json:"pool"`
	IsPrivate            bool   `json:"is_private"`
	InputTokens          int    `json:"input_tokens"`
	OutputTokens         int    `json:"output_tokens"`
	CacheReadTokens      int    `json:"cache_read_tokens"`
	CacheCreateTokens    int    `json:"cache_create_tokens"`
	CostCents            int    `json:"cost_cents"` // total in minor unit of Currency (e.g. USD cents, CNY fen)
	InputCostCents       int    `json:"input_cost_cents"`
	OutputCostCents      int    `json:"output_cost_cents"`
	CacheReadCostCents   int    `json:"cache_read_cost_cents"`
	CacheCreateCostCents int    `json:"cache_create_cost_cents"`
	Currency             string `json:"currency"`
	CostSource           string `json:"cost_source"` // "provider_usage" | "estimated"
}

// Calculator computes cost from usage data and pricing config.
type Calculator struct {
	pricing atomic.Pointer[config.PricingConfig]
}

// NewCalculator creates a Calculator.
func NewCalculator(pricing *config.PricingConfig) *Calculator {
	c := &Calculator{}
	c.pricing.Store(pricing)
	return c
}

// SetPricing atomically updates the pricing table.
// Safe for concurrent use with Calculate; intended for hot-reload paths.
func (c *Calculator) SetPricing(pricing *config.PricingConfig) {
	c.pricing.Store(pricing)
}

// Calculate computes the cost for a vendor/model and token counts, including cache tokens.
// Returns total cost, per-bucket costs, currency, and cost source.
func (c *Calculator) Calculate(vendor, model string, inputTokens, outputTokens, cacheReadTokens, cacheCreateTokens int) (totalCents, inputCostCents, outputCostCents, cacheReadCostCents, cacheCreateCostCents int, currency string, source string) {
	price := c.lookupPrice(vendor, model)
	if price == nil {
		return 0, 0, 0, 0, 0, "USD", "estimated"
	}
	currency = price.Currency
	if currency == "" {
		currency = "USD"
	}

	inputCostF := float64(inputTokens) * price.InputPricePer1KTokens / 1000
	outputCostF := float64(outputTokens) * price.OutputPricePer1KTokens / 1000
	cacheReadCostF := float64(cacheReadTokens) * effectiveCacheReadPrice(price) / 1000
	cacheCreateCostF := float64(cacheCreateTokens) * effectiveCacheCreatePrice(price) / 1000

	inputCost, outputCost, cacheReadCost, cacheCreateCost := int(inputCostF), int(outputCostF), int(cacheReadCostF), int(cacheCreateCostF)

	source = "provider_usage"
	// When cache tokens exist but no cache price is explicitly configured,
	// fallback uses input price — mark estimated to flag the gap.
	hasCacheReadPrice := price.CacheReadPricePer1KTokens != nil
	hasCacheCreatePrice := price.CacheCreatePricePer1KTokens != nil
	if (cacheReadTokens > 0 && !hasCacheReadPrice) || (cacheCreateTokens > 0 && !hasCacheCreatePrice) {
		source = "estimated"
	}

	total := int(inputCostF + outputCostF + cacheReadCostF + cacheCreateCostF)
	return total, inputCost, outputCost, cacheReadCost, cacheCreateCost, currency, source
}

// effectiveCacheReadPrice returns the cache read price, falling back to input price if not explicitly set.
func effectiveCacheReadPrice(m *config.ModelPricing) float64 {
	if m.CacheReadPricePer1KTokens != nil {
		return *m.CacheReadPricePer1KTokens
	}
	return m.InputPricePer1KTokens
}

// effectiveCacheCreatePrice returns the cache create price, falling back to input price if not explicitly set.
func effectiveCacheCreatePrice(m *config.ModelPricing) float64 {
	if m.CacheCreatePricePer1KTokens != nil {
		return *m.CacheCreatePricePer1KTokens
	}
	return m.InputPricePer1KTokens
}

// BuildEvent constructs a CostEvent from usage data.
func BuildEvent(usage *ir.Usage, wire, vendorID, endpointID, model, pool string, isPrivate bool, calc *Calculator) *Event {
	if usage == nil {
		usage = &ir.Usage{}
	}
	totalCents, inputCost, outputCost, cacheReadCost, cacheCreateCost, currency, source := calc.Calculate(
		vendorID, model, usage.Input, usage.Output, usage.CacheRead, usage.CacheCreate,
	)
	return &Event{
		Wire:                 wire,
		VendorID:             vendorID,
		EndpointID:           endpointID,
		Model:                model,
		Pool:                 pool,
		IsPrivate:            isPrivate,
		InputTokens:          usage.Input,
		OutputTokens:         usage.Output,
		CacheReadTokens:      usage.CacheRead,
		CacheCreateTokens:    usage.CacheCreate,
		CostCents:            totalCents,
		InputCostCents:       inputCost,
		OutputCostCents:      outputCost,
		CacheReadCostCents:   cacheReadCost,
		CacheCreateCostCents: cacheCreateCost,
		Currency:             currency,
		CostSource:           source,
	}
}

func (c *Calculator) lookupPrice(vendor, model string) *config.ModelPricing {
	pricing := c.pricing.Load()
	if pricing == nil {
		return nil
	}
	for i := range pricing.Models {
		p := &pricing.Models[i]
		if p.Vendor == vendor && p.Model == model {
			return p
		}
	}
	return nil
}
