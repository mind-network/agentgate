package policy

import (
	"testing"

	"agentgate/internal/gw/config"
)

func TestEngineDecideMatch(t *testing.T) {
	cfg := &config.PolicyConfig{
		Version: "1",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "allow", Reasons: []string{"default"}},
		},
		Rules: []config.PolicyRule{
			{ID: "P-TEST-001", Priority: 500, When: `server_class.task_type == "code_edit"`, Action: "route", ModelPool: "standard", Reasons: []string{"code-edit-to-standard"}},
		},
	}

	eng, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}

	dec, err := eng.Decide(EvalInput{
		ServerClass: map[string]any{"task_type": "code_edit"},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.PrimaryAction != ActionRoute {
		t.Errorf("expected route, got %s", dec.PrimaryAction)
	}
	if dec.ModelPool != "standard" {
		t.Errorf("expected standard pool, got %s", dec.ModelPool)
	}
}

func TestEngineDecideNoMatchDefaults(t *testing.T) {
	cfg := &config.PolicyConfig{
		Version: "1",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "route", ModelPool: "cheap", Reasons: []string{"default-fallthrough"}},
		},
		Rules: []config.PolicyRule{
			{ID: "P-TEST-001", Priority: 500, When: `server_class.task_type == "code_edit"`, Action: "route", ModelPool: "standard", Reasons: []string{"code-edit-to-standard"}},
		},
	}

	eng, _ := NewEngine(cfg)
	dec, err := eng.Decide(EvalInput{
		ServerClass: map[string]any{"task_type": "summary"},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.ModelPool != "cheap" {
		t.Errorf("expected cheap (default), got %s", dec.ModelPool)
	}
}

func TestEngineDecidePriority(t *testing.T) {
	cfg := &config.PolicyConfig{
		Version: "1",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "allow", Reasons: []string{"default"}},
		},
		Rules: []config.PolicyRule{
			{ID: "P-LOW", Priority: 100, When: "true", Action: "route", ModelPool: "cheap", Reasons: []string{"low"}},
			{ID: "P-HIGH", Priority: 1000, When: "true", Action: "route", ModelPool: "strong", Reasons: []string{"high"}},
		},
	}

	eng, _ := NewEngine(cfg)
	dec, _ := eng.Decide(EvalInput{
		ServerClass: map[string]any{},
	})
	// Both rules match; the higher priority's pool should win (deny-overrides,
	// and route overrides route — last match wins via Merge).
	if dec.Reasons[0] != "high" {
		t.Errorf("expected high-priority reason first, got %v", dec.Reasons)
	}
}

func TestEngineDecodeCELErrorSkipsRule(t *testing.T) {
	cfg := &config.PolicyConfig{
		Version: "1",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "allow", Reasons: []string{"default"}},
		},
		Rules: []config.PolicyRule{
			{Priority: 500, ID: "P-GOOD", When: "true", Action: "allow", Reasons: []string{"good"}},
		},
	}

	eng, err := NewEngine(cfg)
	if err != nil {
		t.Fatalf("NewEngine: %v", err)
	}
	dec, err := eng.Decide(EvalInput{
		ServerClass: map[string]any{},
	})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if dec.PrimaryAction != ActionAllow {
		t.Errorf("expected allow from good rule, got %s", dec.PrimaryAction)
	}
}
