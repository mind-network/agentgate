package policy

import (
	"fmt"
	"sort"
	"sync"

	"github.com/google/cel-go/cel"

	"agentgate/internal/gw/config"
)

// Engine evaluates CEL policy rules and produces a three-slot Decision.
type Engine struct {
	mu            sync.RWMutex
	compiledRules []compiledRule
	defaultAction *config.ActionSpec
}

// SetPolicy recompiles all policy rules from a new config and atomically swaps
// the engine's internal state. Safe for concurrent use with Decide.
func (e *Engine) SetPolicy(policyCfg *config.PolicyConfig) error {
	compiled := make([]compiledRule, 0, len(policyCfg.Rules))
	for i, rule := range policyCfg.Rules {
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
			return fmt.Errorf("rule[%d] (%s): create CEL env: %w", i, rule.ID, err)
		}
		ast, issues := env.Compile(rule.When)
		if issues != nil && issues.Err() != nil {
			return fmt.Errorf("rule[%d] (%s): CEL compile: %w", i, rule.ID, issues.Err())
		}
		compiled = append(compiled, compiledRule{rule: &policyCfg.Rules[i], ast: ast})
	}
	sort.Slice(compiled, func(i, j int) bool {
		return compiled[i].rule.Priority > compiled[j].rule.Priority
	})

	e.mu.Lock()
	e.compiledRules = compiled
	e.defaultAction = &policyCfg.Defaults.OnNoMatch
	e.mu.Unlock()
	return nil
}

type compiledRule struct {
	rule *config.PolicyRule
	ast  *cel.Ast
}

// NewEngine compiles all policy rules and returns a ready-to-use engine.
func NewEngine(policyCfg *config.PolicyConfig) (*Engine, error) {
	e := &Engine{
		defaultAction: &policyCfg.Defaults.OnNoMatch,
	}
	for i, rule := range policyCfg.Rules {
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
			return nil, fmt.Errorf("rule[%d] (%s): create CEL env: %w", i, rule.ID, err)
		}
		ast, issues := env.Compile(rule.When)
		if issues != nil && issues.Err() != nil {
			return nil, fmt.Errorf("rule[%d] (%s): CEL compile: %w", i, rule.ID, issues.Err())
		}
		e.compiledRules = append(e.compiledRules, compiledRule{rule: &policyCfg.Rules[i], ast: ast})
	}
	sort.Slice(e.compiledRules, func(i, j int) bool {
		return e.compiledRules[i].rule.Priority > e.compiledRules[j].rule.Priority
	})
	return e, nil
}

// EvalInput is the input to the policy engine's Decide method.
type EvalInput struct {
	ServerClass map[string]any
	TaskHints   map[string]any
	Envelope    map[string]any
	User        map[string]any
	Team        map[string]any
	Repo        map[string]any
	Budget      map[string]any
	Endpoints   map[string]any
}

// Decide evaluates all rules against the input and returns a merged Decision.
func (e *Engine) Decide(input EvalInput) (*Decision, error) {
	e.mu.RLock()
	rules := e.compiledRules
	defAction := e.defaultAction
	e.mu.RUnlock()

	var finalDecision *Decision

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
		return nil, fmt.Errorf("create eval env: %w", err)
	}

	for _, cr := range rules {
		prog, err := env.Program(cr.ast)
		if err != nil {
			continue
		}
		vars := map[string]any{
			"server_class": input.ServerClass,
			"task_hints":   input.TaskHints,
			"envelope":     input.Envelope,
			"user":         input.User,
			"team":         input.Team,
			"repo":         input.Repo,
			"budget":       input.Budget,
			"endpoints":    input.Endpoints,
		}
		if !evalSafely(prog, vars) {
			continue
		}

		d := ruleToDecision(cr.rule)
		finalDecision = Merge(finalDecision, d)
	}

	if finalDecision == nil {
		finalDecision = actionToDecision(defAction)
	}
	return finalDecision, nil
}

// evalSafely evaluates a CEL program, recovering from panics caused by
// missing fields on DynType variables at evaluation time.
func evalSafely(prog cel.Program, vars map[string]any) (matched bool) {
	defer func() {
		if r := recover(); r != nil {
			matched = false
		}
	}()
	out, _, err := prog.Eval(vars)
	if err != nil {
		return false
	}
	m, ok := out.Value().(bool)
	return ok && m
}

func ruleToDecision(r *config.PolicyRule) *Decision {
	switch r.Action {
	case "route":
		return RouteDecision(r.ModelPool, r.Reasons...)
	case "allow":
		return AllowDecision(r.Reasons...)
	default:
		return AllowDecision(r.Reasons...)
	}
}

func actionToDecision(a *config.ActionSpec) *Decision {
	switch a.Action {
	case "route":
		return RouteDecision(a.ModelPool, a.Reasons...)
	case "allow":
		return AllowDecision(a.Reasons...)
	default:
		return AllowDecision(a.Reasons...)
	}
}
