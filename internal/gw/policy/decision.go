// Package policy implements the CEL-based policy Decision engine (§7, §8).
package policy

// PrimaryAction is the main decision outcome.
type PrimaryAction string

const (
	ActionAllow PrimaryAction = "allow"
	ActionRoute PrimaryAction = "route"
	// P1+
	ActionBlock           PrimaryAction = "block"
	ActionRedact          PrimaryAction = "redact"
	ActionRequireApproval PrimaryAction = "require_approval"
)

// Modifier is an additive action applied alongside the primary.
type Modifier string

const (
	ModRedact                Modifier = "redact"
	ModEscalateToStrongModel Modifier = "escalate_to_strong_model"
)

// SideEffect is an action that runs in parallel without affecting the response.
type SideEffect string

const (
	SideShadowEval SideEffect = "shadow_eval"
	SideLogOnly    SideEffect = "log_only"
)

// Decision is the three-slot output of the policy engine (§7.3).
type Decision struct {
	PrimaryAction PrimaryAction `json:"primary_action"`
	Modifiers     []Modifier    `json:"modifiers"`
	SideEffects   []SideEffect  `json:"side_effects"`
	ModelPool     string        `json:"model_pool"`
	ShadowPool    string        `json:"shadow_pool,omitempty"`
	Redactions    []string      `json:"redactions,omitempty"`
	Reasons       []string      `json:"reasons"`

	// Constraint fields populated from policy CEL evaluation (§9.1).
	RequiredTrustTier      string   `json:"required_trust_tier,omitempty"`
	RequiredDataResidency  []string `json:"required_data_residency,omitempty"`
	RequiredCapabilities   []string `json:"required_capabilities,omitempty"`
	DegradedFeatures       []string `json:"degraded_features,omitempty"`
}

// AllowDecision returns an allow decision with the given reasons.
func AllowDecision(reasons ...string) *Decision {
	return &Decision{
		PrimaryAction: ActionAllow,
		Reasons:       reasons,
	}
}

// RouteDecision returns a route decision targeting the given pool.
func RouteDecision(pool string, reasons ...string) *Decision {
	return &Decision{
		PrimaryAction: ActionRoute,
		ModelPool:     pool,
		Reasons:       reasons,
	}
}

// Merge applies the three-slot merge algorithm: deny-overrides for PrimaryAction,
// additive accumulation for Modifiers and SideEffects (§8.2).
func Merge(a, b *Decision) *Decision {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := *a

	// PrimaryAction: deny-overrides (block > require_approval > route > allow)
	out.PrimaryAction = mergePrimary(a.PrimaryAction, b.PrimaryAction)

	// Modifiers / SideEffects: cumulative
	out.Modifiers = appendUniqueMod(out.Modifiers, b.Modifiers)
	out.SideEffects = appendUniqueSE(out.SideEffects, b.SideEffects)
	out.Reasons = appendUniqueStr(out.Reasons, b.Reasons)
	if b.ModelPool != "" {
		out.ModelPool = b.ModelPool
	}
	if b.ShadowPool != "" {
		out.ShadowPool = b.ShadowPool
	}
	out.Redactions = appendUniqueStr(out.Redactions, b.Redactions)
	out.DegradedFeatures = appendUniqueStr(out.DegradedFeatures, b.DegradedFeatures)

	// Constraint fields: union
	if b.RequiredTrustTier != "" {
		out.RequiredTrustTier = b.RequiredTrustTier
	}
	if len(b.RequiredDataResidency) > 0 {
		out.RequiredDataResidency = appendUniqueStr(out.RequiredDataResidency, b.RequiredDataResidency)
	}
	if len(b.RequiredCapabilities) > 0 {
		out.RequiredCapabilities = appendUniqueStr(out.RequiredCapabilities, b.RequiredCapabilities)
	}
	return &out
}

func mergePrimary(a, b PrimaryAction) PrimaryAction {
	rank := map[PrimaryAction]int{
		ActionBlock:           5,
		ActionRequireApproval: 4,
		ActionRoute:           3,
		ActionAllow:           2,
		"":                    1,
	}
	if rank[a] >= rank[b] {
		return a
	}
	return b
}

func appendUniqueMod(slice []Modifier, items []Modifier) []Modifier {
	seen := make(map[Modifier]bool)
	for _, m := range slice {
		seen[m] = true
	}
	for _, m := range items {
		if !seen[m] {
			slice = append(slice, m)
			seen[m] = true
		}
	}
	return slice
}

func appendUniqueSE(slice []SideEffect, items []SideEffect) []SideEffect {
	seen := make(map[SideEffect]bool)
	for _, s := range slice {
		seen[s] = true
	}
	for _, s := range items {
		if !seen[s] {
			slice = append(slice, s)
			seen[s] = true
		}
	}
	return slice
}

func appendUniqueStr(slice, items []string) []string {
	seen := make(map[string]bool)
	for _, s := range slice {
		seen[s] = true
	}
	for _, s := range items {
		if !seen[s] {
			slice = append(slice, s)
			seen[s] = true
		}
	}
	return slice
}
