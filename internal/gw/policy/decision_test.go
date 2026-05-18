package policy

import (
	"testing"
)

func TestMergeDenyOverrides(t *testing.T) {
	allow := AllowDecision("reason-allow")
	route := RouteDecision("standard", "reason-route")

	result := Merge(allow, route)
	if result.PrimaryAction != ActionRoute {
		t.Errorf("route should override allow, got %s", result.PrimaryAction)
	}
}

func TestMergeRouteOverAllow(t *testing.T) {
	a := AllowDecision("r1")
	b := RouteDecision("cheap", "r2")
	out := Merge(a, b)
	if out.PrimaryAction != ActionRoute {
		t.Errorf("expected route, got %s", out.PrimaryAction)
	}
	if out.ModelPool != "cheap" {
		t.Errorf("expected cheap pool, got %s", out.ModelPool)
	}
}

func TestMergeCumulativeReasons(t *testing.T) {
	a := RouteDecision("standard", "r1")
	b := RouteDecision("standard", "r2")
	out := Merge(a, b)
	if len(out.Reasons) != 2 {
		t.Errorf("expected 2 reasons, got %d: %v", len(out.Reasons), out.Reasons)
	}
}

func TestMergeNilReturnsOther(t *testing.T) {
	d := AllowDecision("test")
	if Merge(nil, d) != d {
		t.Error("Merge(nil, d) should return d")
	}
	if Merge(d, nil) != d {
		t.Error("Merge(d, nil) should return d")
	}
}

func TestMergeDedupModifiers(t *testing.T) {
	a := &Decision{PrimaryAction: ActionRoute, Modifiers: []Modifier{ModRedact}}
	b := &Decision{PrimaryAction: ActionRoute, Modifiers: []Modifier{ModRedact, ModEscalateToStrongModel}}
	out := Merge(a, b)
	if len(out.Modifiers) != 2 {
		t.Errorf("expected 2 unique modifiers, got %d", len(out.Modifiers))
	}
}

func TestMergeAccumulatesSideEffects(t *testing.T) {
	a := &Decision{PrimaryAction: ActionRoute, SideEffects: []SideEffect{SideLogOnly}}
	b := &Decision{PrimaryAction: ActionRoute, SideEffects: []SideEffect{SideShadowEval}}
	out := Merge(a, b)
	if len(out.SideEffects) != 2 {
		t.Errorf("expected 2 side effects, got %d", len(out.SideEffects))
	}
}
