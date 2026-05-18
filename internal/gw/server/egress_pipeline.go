package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"agentgate/internal/gw/audit"
	"agentgate/internal/gw/cost"
	"agentgate/internal/gw/policy"
	"agentgate/internal/gw/provider"
	"agentgate/internal/gw/routing"
	"agentgate/internal/shared/ir"
	"agentgate/internal/shared/transformer"
)

// EgressPipeline handles steps 16-23: routing, provider call, streaming tap,
// aicg.usage injection, and event writes.
type EgressPipeline struct {
	Adapters map[string]provider.Adapter
	Calc     *cost.Calculator
	Audit    *audit.Writer
	Selector *routing.Selector
}

// NewEgressPipeline creates an egress pipeline.
func NewEgressPipeline(adapters map[string]provider.Adapter, calc *cost.Calculator, auditW *audit.Writer, sel *routing.Selector) *EgressPipeline {
	return &EgressPipeline{
		Adapters: adapters,
		Calc:     calc,
		Audit:    auditW,
		Selector: sel,
	}
}

// EgressResult bundles the response body and metadata from egress.
type EgressResult struct {
	Body      io.ReadCloser
	CostEvent *cost.Event
	Seed      []byte
	Chain     []routing.Member
	Degraded  []string
	Audit     *audit.Writer
	Member    routing.Member
	TraceID   string
	TenantID  string
	UserID    string
	TeamID    string
	AttemptNo int
	APIKey    string
}

// Run executes the egress pipeline for a streaming request.
// P0: single attempt with pre-first-byte retry once on the same endpoint.
func (p *EgressPipeline) Run(traceID, tenantID, userID, teamID, poolName string, attemptNo int, reqBody []byte, endpointURL, apiKey string, dec *policy.Decision) (*EgressResult, error) {
	chain, err := p.Selector.BuildChain(poolName, dec, traceID, attemptNo)
	if err != nil {
		p.Audit.Write(audit.NewEvent(audit.EventRoutingNoCandidate, traceID, tenantID, userID, teamID))
		return nil, err
	}
	member := chain[0]

	adapter, ok := p.Adapters[member.Wire]
	if !ok {
		// defense-in-depth, redundant with routing.ValidatePoolAdapters at boot
		return nil, fmt.Errorf("egress: no adapter registered for wire %q (endpoint %s)",
			member.Wire, member.EndpointID)
	}

	p.Audit.Write(audit.Event{
		EventType: audit.EventProviderCall,
		TraceID:   traceID,
		TenantID:  tenantID,
		UserID:    userID,
		TeamID:    teamID,
		RoutedTo:  fmt.Sprintf("%s:%s", member.EndpointID, member.Model),
	})

	irReq, err := transformer.AnthropicToIR(reqBody)
	if err != nil {
		return nil, &BadRequestError{Message: "parse anthropic request", Cause: err}
	}

	var degraded []string
	if member.Wire != "anthropic" {
		degraded = transformer.StripAnthropicFeatures(irReq)
	}

	var msgBytes []byte
	var provReq provider.ProviderRequest
	provReq.Model = member.Model
	provReq.MaxTokens = irReq.MaxTokens
	provReq.Temperature = irReq.Temperature
	provReq.Stream = true
	provReq.APIKey = apiKey
	provReq.URL = member.URL

	switch member.Wire {
	case "openai", "openai_compat":
		msgBytes, _ = transformer.IRToOpenAI(irReq)
	case "anthropic", "anthropic_compat":
		msgBytes, _ = transformer.IRToAnthropicMessages(irReq)
		provReq.System, _ = transformer.IRToAnthropicSystem(irReq)
		provReq.Tools, _ = transformer.IRToAnthropicTools(irReq)
		provReq.ToolChoice, _ = transformer.IRToAnthropicToolChoice(irReq)
		provReq.Thinking, _ = transformer.IRToAnthropicThinking(irReq)
		provReq.Metadata = irReq.Metadata
	default:
		return nil, fmt.Errorf("egress: unsupported wire %q (endpoint %s)", member.Wire, member.EndpointID)
	}
	provReq.Messages = msgBytes

	// P0: single attempt + one automatic retry on pre-first-byte 5xx (§5.4.4).
	body, err := p.tryStream(context.TODO(), adapter, &provReq)
	if err != nil {
		var upErr *provider.UpstreamError
		if errors.As(err, &upErr) {
			return nil, err // 4xx: propagate as-is
		}
		p.Audit.Write(audit.Event{
			EventType: audit.EventProvider5xxRetry,
			TraceID:   traceID,
			TenantID:  tenantID,
			UserID:    userID,
			TeamID:    teamID,
			ErrorCode: "provider_5xx_after_retry",
		})
		return nil, fmt.Errorf("egress: provider 5xx after retry: %w", err)
	}

	p.Audit.Write(audit.NewEvent(audit.EventRequestCompleted, traceID, tenantID, userID, teamID))
	slog.Info("egress routed", "trace_id", traceID, "endpoint_id", member.EndpointID, "model", member.Model)

	seed := routing.SeedBytes(traceID, poolName, attemptNo)

	return &EgressResult{
		Body:      body,
		Seed:      seed,
		Chain:     chain,
		Degraded:  degraded,
		Audit:     p.Audit,
		Member:    member,
		TraceID:   traceID,
		TenantID:  tenantID,
		UserID:    userID,
		TeamID:    teamID,
		AttemptNo: attemptNo,
		APIKey:    apiKey,
	}, nil
}

// tryStream attempts a streaming call; on pre-first-byte 5xx, retries once.
// 4xx UpstreamErrors are not retried — they are propagated immediately.
func (p *EgressPipeline) tryStream(ctx context.Context, adapter provider.Adapter, req *provider.ProviderRequest) (io.ReadCloser, error) {
	body, err := adapter.Stream(ctx, req)
	if err != nil {
		var upErr *provider.UpstreamError
		if errors.As(err, &upErr) {
			return nil, err // 4xx: no retry
		}
		// Pre-first-byte failure: retry once on same endpoint.
		slog.Warn("egress: pre-first-byte stream error, retrying", "error", err)
		body2, err2 := adapter.Stream(ctx, req)
		if err2 != nil {
			return nil, fmt.Errorf("retry failed: %w", err2)
		}
		return body2, nil
	}
	return body, nil
}

// InjectUsageIntoResult writes aicg.usage SSE event to the result's body stream
// and returns a new reader that includes the event at the end of the stream.
func InjectUsageIntoResult(result *EgressResult, sessionID string, usage *ir.Usage, costCents int, costSource string, latencyMs int) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		_, _ = io.Copy(pw, result.Body)
		_ = result.Body.Close()
		routedTo := fmt.Sprintf("%s:%s", result.Member.EndpointID, result.Member.Model)
		InjectUsage(pw, result.TraceID, sessionID, usage, costCents, costSource, routedTo, result.AttemptNo, result.Degraded, latencyMs)
	}()
	return pr
}

// InjectUsage writes an aicg.usage SSE event to the stream.
func InjectUsage(w io.Writer, traceID, sessionID string, usage *ir.Usage, costCents int, costSource, routedTo string, attemptNo int, degraded []string, latencyMs int) {
	data := map[string]any{
		"schema_version": "1.0",
		"trace_id":       traceID,
		"session_id":     sessionID,
		"cost_cents":     costCents,
		"cost_source":    costSource,
		"tokens": map[string]int{
			"input":        usage.Input,
			"output":       usage.Output,
			"cache_read":   usage.CacheRead,
			"cache_create": usage.CacheCreate,
		},
		"routed_to":         routedTo,
		"attempt_no":        attemptNo,
		"degraded_features": degraded,
		"latency_ms":        latencyMs,
		"partial":           false,
	}
	b, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: aicg.usage\ndata: %s\n\n", string(b))
}

// InjectError writes an aicg.error SSE event.
func InjectError(w io.Writer, traceID, code, message string, partial bool, tokensEmitted int) {
	data := map[string]any{
		"schema_version":        "1.0",
		"trace_id":              traceID,
		"code":                  code,
		"message":               message,
		"partial":               partial,
		"tokens_emitted_so_far": tokensEmitted,
	}
	b, _ := json.Marshal(data)
	_, _ = fmt.Fprintf(w, "event: aicg.error\ndata: %s\n\n", string(b))
}

// BuildAICGUsageJSON constructs the aicg.usage JSON payload.
func BuildAICGUsageJSON(traceID, sessionID string, ce *cost.Event, attemptNo int, degraded []string, latencyMs int) []byte {
	if ce == nil {
		return nil
	}
	data := map[string]any{
		"schema_version": "1.0",
		"trace_id":       traceID,
		"session_id":     sessionID,
		"cost_cents":     ce.CostCents,
		"cost_source":    ce.CostSource,
		"tokens": map[string]int{
			"input":        ce.InputTokens,
			"output":       ce.OutputTokens,
			"cache_read":   0,
			"cache_create": 0,
		},
		"decision": map[string]any{
			"primary_action": "route",
			"modifiers":      []string{},
			"side_effects":   []string{},
			"model_pool":     ce.Pool,
			"reasons":        []string{},
		},
		"routed_to":         fmt.Sprintf("%s:%s", ce.EndpointID, ce.Model),
		"attempt_no":        attemptNo,
		"degraded_features": degraded,
		"latency_ms":        latencyMs,
		"partial":           false,
	}
	b, _ := json.Marshal(data)
	return b
}
