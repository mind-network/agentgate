package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadPools loads and validates pools.yaml.
func LoadPools(path string) (*PoolsConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pools.yaml: %w", err)
	}
	var cfg PoolsConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse pools.yaml: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate pools.yaml: %w", err)
	}
	return &cfg, nil
}

// Validate checks all pool member endpoint references are in the registry.
func (c *PoolsConfig) Validate() error {
	if len(c.ProviderEndpoints) == 0 {
		return fmt.Errorf("provider_endpoints registry must not be empty")
	}
	for poolName, pool := range c.Pools {
		if len(pool.Members) == 0 {
			return fmt.Errorf("pool %q has no members", poolName)
		}
		for i, m := range pool.Members {
			if _, ok := c.ProviderEndpoints[m.EndpointID]; !ok {
				return fmt.Errorf(
					"pool %q member[%d] endpoint_id %q not found in provider_endpoints registry",
					poolName, i, m.EndpointID,
				)
			}
			if m.Weight <= 0 {
				return fmt.Errorf("pool %q member[%d] weight must be > 0, got %d", poolName, i, m.Weight)
			}
		}
		if pool.TimeoutMs <= 0 {
			return fmt.Errorf("pool %q timeout_ms must be > 0", poolName)
		}
		if pool.MaxAttempts <= 0 {
			return fmt.Errorf("pool %q max_attempts must be > 0", poolName)
		}
	}
	return nil
}
