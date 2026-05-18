package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"agentgate/internal/gw/audit"
	"agentgate/internal/gw/budget"
	"agentgate/internal/gw/classifier"
	"agentgate/internal/gw/config"
	"agentgate/internal/gw/cost"
	"agentgate/internal/gw/edge"
	"agentgate/internal/gw/policy"
	"agentgate/internal/gw/rawstore"
	"agentgate/internal/gw/routing"
)

// PipelineResult holds the output of the ingress pipeline.
type PipelineResult struct {
	StatusCode int
	Headers    http.Header
	Body       io.ReadCloser
}

// Pipeline runs ingress steps 11-15: reclassify → policy → routing → egress.
type Pipeline struct {
	Policy      *policy.Engine
	Selector    *routing.Selector
	Egress      *EgressPipeline
	AuditW      *audit.Writer
	CostCalc    *cost.Calculator
	cfgMu       sync.RWMutex
	cfg         *config.Config
	PgAuditW    *audit.PgWriter
	PgCostW     *cost.PgWriter
	PgRoutingW  *routing.EventWriter
	PgRawstoreW *rawstore.MetadataWriter
	PgBudgetSvc *budget.PgService
}

// Config returns the current config snapshot (concurrency-safe).
func (p *Pipeline) Config() *config.Config {
	p.cfgMu.RLock()
	defer p.cfgMu.RUnlock()
	return p.cfg
}

// SetConfig atomically swaps the current config snapshot (concurrency-safe).
func (p *Pipeline) SetConfig(c *config.Config) {
	p.cfgMu.Lock()
	p.cfg = c
	p.cfgMu.Unlock()
}

// NewPipeline creates a fully-wired ingress pipeline.
func NewPipeline(pol *policy.Engine, sel *routing.Selector, eg *EgressPipeline, auditW *audit.Writer, costCalc *cost.Calculator, cfg *config.Config) *Pipeline {
	return &Pipeline{
		Policy:   pol,
		Selector: sel,
		Egress:   eg,
		AuditW:   auditW,
		CostCalc: costCalc,
		cfg:      cfg,
	}
}

// Run executes the ingress pipeline: classify → policy → routing → egress.
func (p *Pipeline) Run(ctx context.Context, req *ForwardRequest, traceID string) (*PipelineResult, error) {
	userID := edge.GetUserID(ctx)
	teamID := edge.GetTeamID(ctx)
	role := edge.GetRole(ctx)

	// Step 11: Server reclassifier.
	tag := classifier.Reclassify(nil, "", 0, 0, false, false)

	// Step 12: Safety scan — P1, skipped.

	// Step 13: Policy evaluation.
	dec, err := p.Policy.Decide(policy.EvalInput{
		User: map[string]any{
			"user_id": userID,
			"team_id": teamID,
			"role":    role,
		},
		TaskHints: map[string]any{
			"task_type":        tag.TaskType,
			"complexity":       tag.Complexity,
			"data_sensitivity": tag.DataSensitivity,
		},
	})
	if err != nil {
		p.AuditW.Write(audit.NewEvent(audit.EventRoutingNoCandidate, traceID, teamID, userID, teamID))
		return nil, fmt.Errorf("policy evaluate: %w", err)
	}

	p.AuditW.Write(audit.Event{
		EventType: audit.EventDecisionMade,
		TraceID:   traceID,
		TenantID:  teamID,
		UserID:    userID,
		TeamID:    teamID,
	})

	// Detect required capabilities from request body and add to decision constraints.
	// Features configured as degradable are omitted from hard requirements so the
	// standard pool can route to non-anthropic wires.
	if req.Wire.Protocol == "anthropic_messages" {
		detected := policy.DetectCapabilities(req.Wire.Body)
		var allowed []string
		if c := p.Config(); c != nil && c.Policy != nil {
			allowed = c.Policy.Degradation.AllowedCapabilities
		}
		hard := policy.FilterDegradable(detected, allowed)
		dec.RequiredCapabilities = appendUniqueStr(dec.RequiredCapabilities, hard)
	}

	poolName := dec.ModelPool
	if poolName == "" {
		poolName = "standard" // default pool
	}

	// Step 14: Build routing chain.
	members, err := p.Selector.BuildChain(poolName, dec, traceID, 1)
	if err != nil {
		p.AuditW.Write(audit.NewEvent(audit.EventRoutingNoCandidate, traceID, teamID, userID, teamID))
		return nil, fmt.Errorf("routing build chain: %w", err)
	}
	member := members[0]

	// Resolve API key from secret resolver output.
	apiKey := ""
	if ep, ok := p.Config().ResolvedEndpoints[member.EndpointID]; ok {
		apiKey = ep.ResolvedKey
	}

	// Serialize body for egress.
	bodyBytes, _ := json.Marshal(req.Wire.Body)

	// Step 15-23: Egress pipeline (provider call, streaming, aicg.usage).
	t0 := time.Now()
	result, err := p.Egress.Run(traceID, teamID, userID, teamID, poolName, 1, bodyBytes, member.URL, apiKey, dec)
	if err != nil {
		return nil, fmt.Errorf("egress: %w", err)
	}

	// Merge actual degraded features from egress into decision before persisting.
	dec.DegradedFeatures = appendUniqueStr(dec.DegradedFeatures, result.Degraded)

	latencyMs := int(time.Since(t0).Milliseconds())

	// Wrap the provider stream in an AccountedStream that forwards frames
	// in real-time, accumulates usage, writes cost_event at EOF, and
	// injects aicg.usage from the same computed event.
	// No early empty-usage cost_event is written before the stream drains.
	accountedCfg := &AccountedStreamConfig{
		Wire:       member.Wire,
		Calc:       p.CostCalc,
		CostW:      p.PgCostW,
		VendorID:   member.VendorID,
		EndpointID: member.EndpointID,
		Model:      member.Model,
		Pool:       poolName,
		IsPrivate:  member.Private,
		TraceID:    traceID,
		TenantID:   teamID,
		UserID:     userID,
		TeamID:     teamID,
		AttemptNo:  1,
		Degraded:   dec.DegradedFeatures,
		LatencyMs:  latencyMs,
	}

	// Budget: Reserve estimated cost (P0: 100 cents default estimate, soft-warn only).
	if p.PgBudgetSvc != nil {
		rsv, _ := p.PgBudgetSvc.Reserve(ctx, traceID, teamID, userID, teamID, 100)
		if rsv != nil {
			reservationID := rsv.ID
			accountedCfg.PostAccount = func(r *StreamAccountingResult) {
				if r.Err != nil {
					_ = p.PgBudgetSvc.Release(context.Background(), reservationID)
				} else if r.CostEvent != nil {
					_ = p.PgBudgetSvc.Commit(context.Background(), reservationID, r.CostEvent.CostCents)
				}
			}
		}
	}

	accountedStream := NewAccountedStream(result.Body, accountedCfg)

	// Persist pre-accounting events to Postgres (best-effort, non-blocking).
	if p.PgAuditW != nil {
		_ = p.PgAuditW.WriteCtx(ctx, audit.NewEvent(audit.EventRequestCompleted, traceID, teamID, userID, teamID))
	}
	if p.PgRoutingW != nil {
		seed := routing.SeedBytes(traceID, poolName, 1)
		decisionJSON, _ := json.Marshal(dec)
		memberJSON, _ := json.Marshal(member)
		_ = p.PgRoutingW.Write(ctx, traceID, teamID, 1, decisionJSON, poolName, memberJSON, nil, result.Degraded, "", seed)
	}
	if p.PgRawstoreW != nil {
		_ = p.PgRawstoreW.Write(ctx, traceID, teamID, userID, teamID, "", "unknown")
	}

	slog.Info("ingress pipeline completed",
		"trace_id", traceID,
		"pool", poolName,
		"endpoint", member.EndpointID,
		"model", member.Model,
		"latency_ms", latencyMs,
	)

	return &PipelineResult{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":     []string{"text/event-stream"},
			"X-AICG-Trace-Id":  []string{traceID},
			"X-AICG-Routed-To": []string{fmt.Sprintf("%s:%s", member.EndpointID, member.Model)},
		},
		Body: accountedStream,
	}, nil
}

// appendUniqueStr appends items not already present in the slice.
func appendUniqueStr(slice, items []string) []string {
	seen := make(map[string]bool, len(slice))
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
