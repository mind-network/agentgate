package config

import (
	"strings"
	"testing"
)

func TestLoadAndValidateSuccess(t *testing.T) {
	cfg, err := LoadAndValidate("test", []byte(validPolicyYAML))
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if cfg.Version != "1" {
		t.Errorf("version = %q, want \"1\"", cfg.Version)
	}
	if len(cfg.Rules) != 3 {
		t.Errorf("expected 3 rules, got %d", len(cfg.Rules))
	}
}

func TestLoadAndValidateCELCompileError(t *testing.T) {
	badCEL := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["fallback"]
rules:
  - id: P-BAD-001
    description: bad CEL
    priority: 500
    when: "this is not valid CEL (((("
    action: route
    model_pool: cheap
    reasons: ["test"]
`
	_, err := LoadAndValidate("test", []byte(badCEL))
	if err == nil {
		t.Fatal("expected CEL compile error, got nil")
	}
}

func TestLoadAndValidateBlockActionRejected(t *testing.T) {
	badAction := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["fallback"]
rules:
  - id: P-BLOCK-001
    description: block is P1 only
    priority: 1000
    when: "true"
    action: block
    reasons: ["test"]
`
	_, err := LoadAndValidate("test", []byte(badAction))
	if err == nil {
		t.Fatal("expected error for block action in P0, got nil")
	}
	if !strings.Contains(err.Error(), "not allowed in P0") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadAndValidateRedactActionRejected(t *testing.T) {
	badAction := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["fallback"]
rules:
  - id: P-REDACT-001
    description: redact is P1 only
    priority: 990
    when: "true"
    action: redact
    reasons: ["test"]
`
	_, err := LoadAndValidate("test", []byte(badAction))
	if err == nil {
		t.Fatal("expected error for redact action in P0, got nil")
	}
}

func TestLoadAndValidateRouteRequiresModelPool(t *testing.T) {
	noPool := `
version: 1
defaults:
  on_no_match:
    action: allow
    reasons: ["fallback"]
rules:
  - id: P-ROUTE-001
    description: route without pool
    priority: 500
    when: "true"
    action: route
    reasons: ["test"]
`
	_, err := LoadAndValidate("test", []byte(noPool))
	if err == nil {
		t.Fatal("expected error for route without model_pool, got nil")
	}
}

func TestLoadAndValidateDefaultOnNoMatchRequired(t *testing.T) {
	noDefaults := `
version: 1
rules:
  - id: P-R-001
    description: test
    priority: 500
    when: "true"
    action: allow
    reasons: ["test"]
`
	_, err := LoadAndValidate("test", []byte(noDefaults))
	if err == nil {
		t.Fatal("expected error for missing defaults action, got nil")
	}
}

func TestLoadAndValidateRuleIDPrefix(t *testing.T) {
	badID := `
version: 1
defaults:
  on_no_match:
    action: allow
    reasons: ["fallback"]
rules:
  - id: BAD-ID
    description: bad id prefix
    priority: 500
    when: "true"
    action: allow
    reasons: ["test"]
`
	_, err := LoadAndValidate("test", []byte(badID))
	if err == nil {
		t.Fatal("expected error for bad rule id prefix, got nil")
	}
	if !strings.Contains(err.Error(), "must start with 'P-'") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidatePolicyAgainstPoolsSuccess(t *testing.T) {
	policy := &PolicyConfig{
		Version: "1",
		Defaults: Defaults{
			OnNoMatch: ActionSpec{Action: "route", ModelPool: "standard", Reasons: []string{"fallback"}},
		},
		Rules: []PolicyRule{
			{ID: "P-R-001", Priority: 500, When: "true", Action: "route", ModelPool: "cheap"},
			{ID: "P-R-002", Priority: 500, When: "true", Action: "route", ModelPool: "strong"},
		},
	}
	pools := &PoolsConfig{
		Pools: map[string]Pool{
			"cheap":    {Members: []PoolMember{{EndpointID: "ep1", Model: "m1", Weight: 100}}},
			"standard": {Members: []PoolMember{{EndpointID: "ep2", Model: "m2", Weight: 100}}},
			"strong":   {Members: []PoolMember{{EndpointID: "ep3", Model: "m3", Weight: 100}}},
		},
	}
	if err := ValidatePolicyAgainstPools(policy, pools); err != nil {
		t.Errorf("expected success, got: %v", err)
	}
}

func TestValidatePolicyAgainstPoolsMissingPool(t *testing.T) {
	policy := &PolicyConfig{
		Version: "1",
		Defaults: Defaults{
			OnNoMatch: ActionSpec{Action: "route", ModelPool: "standard", Reasons: []string{"fallback"}},
		},
		Rules: []PolicyRule{
			{ID: "P-R-001", Priority: 500, When: "true", Action: "route", ModelPool: "nonexistent_pool"},
		},
	}
	pools := &PoolsConfig{
		Pools: map[string]Pool{
			"standard": {Members: []PoolMember{{EndpointID: "ep1", Model: "m1", Weight: 100}}},
		},
	}
	err := ValidatePolicyAgainstPools(policy, pools)
	if err == nil {
		t.Fatal("expected error for missing pool reference, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent_pool") {
		t.Errorf("error should mention missing pool, got: %v", err)
	}
}

func TestValidatePolicyAgainstPoolsDefaultMissingPool(t *testing.T) {
	policy := &PolicyConfig{
		Version: "1",
		Defaults: Defaults{
			OnNoMatch: ActionSpec{Action: "route", ModelPool: "missing_default_pool", Reasons: []string{"fallback"}},
		},
	}
	pools := &PoolsConfig{
		Pools: map[string]Pool{
			"standard": {Members: []PoolMember{{EndpointID: "ep1", Model: "m1", Weight: 100}}},
		},
	}
	err := ValidatePolicyAgainstPools(policy, pools)
	if err == nil {
		t.Fatal("expected error for missing default pool reference, got nil")
	}
	if !strings.Contains(err.Error(), "missing_default_pool") {
		t.Errorf("error should mention missing pool, got: %v", err)
	}
}

func TestValidatePolicyAgainstPoolsAllowActionSkipsModelPoolCheck(t *testing.T) {
	policy := &PolicyConfig{
		Version: "1",
		Defaults: Defaults{
			OnNoMatch: ActionSpec{Action: "allow", Reasons: []string{"fallback"}},
		},
		Rules: []PolicyRule{
			{ID: "P-R-001", Priority: 500, When: "true", Action: "allow"},
		},
	}
	pools := &PoolsConfig{
		Pools: map[string]Pool{},
	}
	if err := ValidatePolicyAgainstPools(policy, pools); err != nil {
		t.Errorf("allow action should not trigger pool check, got: %v", err)
	}
}

func TestDegradationValidConfig(t *testing.T) {
	yaml := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["fallback"]
degradation:
  allowed_capabilities: ["cache_control", "extended_thinking"]
rules:
  - id: P-R-001
    description: test
    priority: 500
    when: "true"
    action: route
    model_pool: standard
    reasons: ["test"]
`
	cfg, err := LoadAndValidate("test", []byte(yaml))
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if len(cfg.Degradation.AllowedCapabilities) != 2 {
		t.Errorf("expected 2 allowed capabilities, got %d", len(cfg.Degradation.AllowedCapabilities))
	}
	if cfg.Degradation.AllowedCapabilities[0] != "cache_control" || cfg.Degradation.AllowedCapabilities[1] != "extended_thinking" {
		t.Errorf("unexpected allowed capabilities: %v", cfg.Degradation.AllowedCapabilities)
	}
}

func TestDegradationMissingConfigIsValid(t *testing.T) {
	yaml := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["fallback"]
rules:
  - id: P-R-001
    description: test
    priority: 500
    when: "true"
    action: route
    model_pool: standard
    reasons: ["test"]
`
	cfg, err := LoadAndValidate("test", []byte(yaml))
	if err != nil {
		t.Fatalf("expected success with missing degradation, got: %v", err)
	}
	if len(cfg.Degradation.AllowedCapabilities) != 0 {
		t.Errorf("expected empty allowed capabilities when missing, got %v", cfg.Degradation.AllowedCapabilities)
	}
}

func TestDegradationEmptyCapabilitiesIsValid(t *testing.T) {
	yaml := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["fallback"]
degradation:
  allowed_capabilities: []
rules:
  - id: P-R-001
    description: test
    priority: 500
    when: "true"
    action: route
    model_pool: standard
    reasons: ["test"]
`
	cfg, err := LoadAndValidate("test", []byte(yaml))
	if err != nil {
		t.Fatalf("expected success with empty degradation list, got: %v", err)
	}
	if len(cfg.Degradation.AllowedCapabilities) != 0 {
		t.Errorf("expected empty allowed capabilities, got %v", cfg.Degradation.AllowedCapabilities)
	}
}

func TestDegradationRejectsInvalidCapability(t *testing.T) {
	yaml := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["fallback"]
degradation:
  allowed_capabilities: ["cache_control", "vision"]
rules:
  - id: P-R-001
    description: test
    priority: 500
    when: "true"
    action: route
    model_pool: standard
    reasons: ["test"]
`
	_, err := LoadAndValidate("test", []byte(yaml))
	if err == nil {
		t.Fatal("expected error for invalid degradation capability 'vision'")
	}
	if !strings.Contains(err.Error(), "vision") {
		t.Errorf("error should mention 'vision', got: %v", err)
	}
	if !strings.Contains(err.Error(), "degradation") {
		t.Errorf("error should mention 'degradation', got: %v", err)
	}
}

func TestDegradationRejectsUnknownCapability(t *testing.T) {
	yaml := `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["fallback"]
degradation:
  allowed_capabilities: ["streaming"]
rules:
  - id: P-R-001
    description: test
    priority: 500
    when: "true"
    action: route
    model_pool: standard
    reasons: ["test"]
`
	_, err := LoadAndValidate("test", []byte(yaml))
	if err == nil {
		t.Fatal("expected error for unknown degradation capability 'streaming'")
	}
	if !strings.Contains(err.Error(), "streaming") {
		t.Errorf("error should mention 'streaming', got: %v", err)
	}
}

const validPolicyYAML = `
version: 1
defaults:
  on_no_match:
    action: route
    model_pool: standard
    reasons: ["default-fallthrough"]
rules:
  - id: P-ROUTE-001
    description: "summary / test_output → cheap"
    priority: 500
    when: 'server_class.task_type in ["summary","test_output"]'
    action: route
    model_pool: cheap
    reasons: ["low-complexity-to-cheap"]
  - id: P-ROUTE-002
    description: "code_edit → standard"
    priority: 500
    when: 'server_class.task_type in ["code_edit","simple_edit"]'
    action: route
    model_pool: standard
    reasons: ["standard-work-to-standard"]
  - id: P-ROUTE-003
    description: "debug → strong"
    priority: 500
    when: 'server_class.task_type in ["architecture","debug"]'
    action: route
    model_pool: strong
    reasons: ["complex-work-to-strong"]
`
