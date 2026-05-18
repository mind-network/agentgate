package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadPricing loads pricing.yaml.
func LoadPricing(path string) (*PricingConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pricing.yaml: %w", err)
	}
	var cfg PricingConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse pricing.yaml: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate pricing.yaml: %w", err)
	}
	return &cfg, nil
}

// Validate checks the pricing config: required fields, non-negative prices,
// valid currency codes, and (vendor, model) uniqueness. Models with
// cache_control:true must declare cache read and create prices.
func (c *PricingConfig) Validate() error {
	if len(c.Models) == 0 {
		return fmt.Errorf("at least one model pricing entry is required")
	}
	seen := make(map[string]int) // "vendor:model" -> first index
	for i, m := range c.Models {
		if m.Vendor == "" {
			return fmt.Errorf("model[%d]: vendor is required", i)
		}
		if m.Model == "" {
			return fmt.Errorf("model[%d]: model name is required", i)
		}
		if m.InputPricePer1KTokens < 0 || m.OutputPricePer1KTokens < 0 {
			return fmt.Errorf("model[%d] (%s/%s): prices must be >= 0", i, m.Vendor, m.Model)
		}
		if m.Currency != "" && len(m.Currency) != 3 {
			return fmt.Errorf("model[%d] (%s/%s): currency must be a 3-letter ISO 4217 code", i, m.Vendor, m.Model)
		}
		// cache-capable models must declare cache prices.
		if m.Capabilities.CacheControl {
			if m.CacheReadPricePer1KTokens == nil {
				return fmt.Errorf("model[%d] (%s/%s): cache_read_price_per_1k_tokens is required for cache-capable models", i, m.Vendor, m.Model)
			}
			if m.CacheCreatePricePer1KTokens == nil {
				return fmt.Errorf("model[%d] (%s/%s): cache_create_price_per_1k_tokens is required for cache-capable models", i, m.Vendor, m.Model)
			}
		}
		// Reject negative cache prices whenever present, regardless of cache_control.
		if m.CacheReadPricePer1KTokens != nil && *m.CacheReadPricePer1KTokens < 0 {
			return fmt.Errorf("model[%d] (%s/%s): cache_read_price_per_1k_tokens must be >= 0", i, m.Vendor, m.Model)
		}
		if m.CacheCreatePricePer1KTokens != nil && *m.CacheCreatePricePer1KTokens < 0 {
			return fmt.Errorf("model[%d] (%s/%s): cache_create_price_per_1k_tokens must be >= 0", i, m.Vendor, m.Model)
		}
		key := m.Vendor + ":" + m.Model
		if first, ok := seen[key]; ok {
			return fmt.Errorf("model[%d] (%s/%s): duplicate (vendor, model) pair; first seen at model[%d]", i, m.Vendor, m.Model, first)
		}
		seen[key] = i
	}
	return nil
}

// Lookup returns the pricing row for a given (vendor, model) pair, or nil if not found.
func (c *PricingConfig) Lookup(vendor, model string) *ModelPricing {
	for i := range c.Models {
		if c.Models[i].Vendor == vendor && c.Models[i].Model == model {
			return &c.Models[i]
		}
	}
	return nil
}

// ValidateEndpointsAgainstPricing checks that every endpoint's vendor appears in at
// least one pricing row that matches a model used by some pool member of that endpoint.
// Endpoints with no pool members are skipped (vacuously valid).
func ValidateEndpointsAgainstPricing(pools *PoolsConfig, pricing *PricingConfig) error {
	priced := make(map[string]bool, len(pricing.Models))
	for _, mp := range pricing.Models {
		priced[mp.Vendor+":"+mp.Model] = true
	}
	vendorsInPricing := make(map[string]bool)
	for _, mp := range pricing.Models {
		vendorsInPricing[mp.Vendor] = true
	}

	for endpointID, ep := range pools.ProviderEndpoints {
		hasPricedModel := false
		hasAnyMember := false
		for _, pool := range pools.Pools {
			for _, m := range pool.Members {
				if m.EndpointID == endpointID {
					hasAnyMember = true
					if priced[ep.Vendor+":"+m.Model] {
						hasPricedModel = true
						break
					}
				}
			}
			if hasPricedModel {
				break
			}
		}
		if !hasAnyMember {
			continue
		}
		if !vendorsInPricing[ep.Vendor] {
			return fmt.Errorf(
				"provider_endpoints.%s: vendor %q has no pricing entries; add at least one (vendor, model) row in pricing.yaml for this vendor",
				endpointID, ep.Vendor,
			)
		}
		if !hasPricedModel {
			return fmt.Errorf(
				"provider_endpoints.%s: vendor %q has no pricing row for any model used by pool members of this endpoint",
				endpointID, ep.Vendor,
			)
		}
	}
	return nil
}

// DefaultCurrencies fills in the default "USD" currency for any entry that omits it.
func (c *PricingConfig) DefaultCurrencies() {
	for i := range c.Models {
		if c.Models[i].Currency == "" {
			c.Models[i].Currency = "USD"
		}
	}
}
