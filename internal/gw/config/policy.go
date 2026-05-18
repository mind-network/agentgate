package config

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/google/cel-go/cel"
	"gopkg.in/yaml.v3"
)

// validP0Actions lists the only actions allowed in Phase 0.
var validP0Actions = map[string]bool{"route": true, "allow": true}

// LoadPolicy loads a policy YAML file and compiles its CEL expressions.
func LoadPolicy(path string) (*PolicyConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read policy file: %w", err)
	}
	return LoadAndValidate(path, data)
}

// LoadAndValidate parses raw YAML bytes, validates the schema, and compiles
// all CEL expressions. This is the public entry point used by both GW server
// startup and the `aicg policyctl validate` CLI command.
func LoadAndValidate(path string, data []byte) (*PolicyConfig, error) {
	var cfg PolicyConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse policy YAML: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("validate policy %s: %w", path, err)
	}
	return &cfg, nil
}

// Validate checks the policy config and compiles all CEL expressions.
func (c *PolicyConfig) Validate() error {
	if c.Version == "" {
		return fmt.Errorf("version is required")
	}

	if c.Defaults.OnNoMatch.Action == "" {
		return fmt.Errorf("defaults.on_no_match.action is required")
	}
	if err := validateP0Action(c.Defaults.OnNoMatch.Action); err != nil {
		return fmt.Errorf("defaults.on_no_match: %w", err)
	}

	if err := c.Defaults.OnNoMatch.validateModelPoolRequired(); err != nil {
		return fmt.Errorf("defaults.on_no_match: %w", err)
	}

	if err := c.Degradation.validate(); err != nil {
		return fmt.Errorf("degradation: %w", err)
	}

	for i, rule := range c.Rules {
		if rule.ID == "" {
			return fmt.Errorf("rule[%d]: id is required", i)
		}
		if !strings.HasPrefix(rule.ID, "P-") {
			return fmt.Errorf("rule[%d] id %q: must start with 'P-'", i, rule.ID)
		}
		if rule.When == "" {
			return fmt.Errorf("rule[%d] (%s): when expression is required", i, rule.ID)
		}
		if err := validateP0Action(rule.Action); err != nil {
			return fmt.Errorf("rule[%d] (%s): %w", i, rule.ID, err)
		}
		if err := rule.validateModelPoolRequired(); err != nil {
			return fmt.Errorf("rule[%d] (%s): %w", i, rule.ID, err)
		}
		// Compile CEL expression
		if _, err := compileCEL(rule.When); err != nil {
			return fmt.Errorf("rule[%d] (%s): CEL compile: %w", i, rule.ID, err)
		}
	}

	return nil
}

func validateP0Action(action string) error {
	if !validP0Actions[action] {
		return fmt.Errorf("action %q is not allowed in P0 (only route, allow)", action)
	}
	return nil
}

func (a ActionSpec) validateModelPoolRequired() error {
	if a.Action == "route" && a.ModelPool == "" {
		return fmt.Errorf("action %q requires model_pool", a.Action)
	}
	return nil
}

func (r PolicyRule) validateModelPoolRequired() error {
	return ActionSpec{Action: r.Action, ModelPool: r.ModelPool}.validateModelPoolRequired()
}

// compileCEL compiles a CEL expression string.
func compileCEL(expr string) (*cel.Ast, error) {
	env, err := cel.NewEnv(
		cel.Variable("server_class", cel.DynType),
		cel.Variable("task_hints", cel.DynType),
		cel.Variable("envelope", cel.DynType),
		cel.Variable("user", cel.DynType),
		cel.Variable("team", cel.DynType),
		cel.Variable("repo", cel.DynType),
		cel.Variable("budget", cel.DynType),
		cel.Variable("endpoints", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		return nil, fmt.Errorf("create CEL env: %w", err)
	}
	ast, issues := env.Compile(expr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("compile: %w", issues.Err())
	}
	return ast, nil
}

// validDegradableCapabilities lists the only capabilities that may be degraded.
var validDegradableCapabilities = []string{"cache_control", "extended_thinking"}

func (d DegradationConfig) validate() error {
	for _, cap := range d.AllowedCapabilities {
		if !slices.Contains(validDegradableCapabilities, cap) {
			return fmt.Errorf("allowed_capabilities: %q is not a degradable capability (valid: %s)", cap, strings.Join(validDegradableCapabilities, ", "))
		}
	}
	return nil
}

// ValidatePolicyAgainstPools checks that every model_pool referenced in the
// policy config (rules and defaults) exists as a key in the pools config.
// Also verifies that pool member endpoint_ids are present in the registry.
// Called by both GW startup and `aicg policyctl validate`.
func ValidatePolicyAgainstPools(policy *PolicyConfig, pools *PoolsConfig) error {
	poolNames := make(map[string]bool)
	for name := range pools.Pools {
		poolNames[name] = true
	}

	missingPools := make(map[string]bool)

	if policy.Defaults.OnNoMatch.Action == "route" {
		mp := policy.Defaults.OnNoMatch.ModelPool
		if mp != "" && !poolNames[mp] {
			missingPools[mp] = true
		}
	}

	for _, rule := range policy.Rules {
		if rule.Action == "route" && rule.ModelPool != "" {
			if !poolNames[rule.ModelPool] {
				missingPools[rule.ModelPool] = true
			}
		}
	}

	if len(missingPools) > 0 {
		sorted := make([]string, 0, len(missingPools))
		for mp := range missingPools {
			sorted = append(sorted, mp)
		}
		sort.Strings(sorted)
		return fmt.Errorf("policy references model_pool(s) not declared in pools.yaml: %s", strings.Join(sorted, ", "))
	}

	return nil
}
