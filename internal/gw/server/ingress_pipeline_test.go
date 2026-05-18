package server

import (
	"context"
	"io"
	"strings"
	"testing"

	"agentgate/internal/gw/audit"
	"agentgate/internal/gw/config"
	"agentgate/internal/gw/cost"
	"agentgate/internal/gw/policy"
	"agentgate/internal/gw/provider"
	"agentgate/internal/gw/routing"
)

// TestPipelineRunWrapsBodyInAccountedStream verifies that Pipeline.Run wraps the
// provider stream body in an AccountedStream instead of building an empty-usage
// cost event. The AccountedStream forwards frames in real-time and accumulates
// usage, so consumers see forwarded data only after draining.
func TestPipelineRunWrapsBodyInAccountedStream(t *testing.T) {
	// SSE stream with Anthropic usage data (cache tokens + cumulative output).
	sse := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"output_tokens\":5,\"cache_read_input_tokens\":50,\"cache_creation_input_tokens\":20}}}\n\nevent: content_block_delta\ndata: {\"type\":\"text_delta\",\"text\":\"Hello\"}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	adapter := &countingAdapter{name: "anthropic", body: sse}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"anthropic-prod": {Wire: "anthropic", Vendor: "anthropic", URL: "https://api.anthropic.com"},
		},
		Pools: map[string]config.Pool{
			"standard": {
				Members:     []config.PoolMember{{EndpointID: "anthropic-prod", Model: "claude-sonnet-4-6", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, nil)

	pol, _ := policy.NewEngine(&config.PolicyConfig{
		Version: "1.0",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "route", ModelPool: "standard"},
		},
	})

	pricing := &config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor:                      "anthropic",
				Model:                       "claude-sonnet-4-6",
				InputPricePer1KTokens:       3.0,
				OutputPricePer1KTokens:      15.0,
				CacheReadPricePer1KTokens:   float64Ptr(0.30),
				CacheCreatePricePer1KTokens: float64Ptr(3.0),
				Capabilities:                config.ModelCapabilities{Tools: true, CacheControl: true},
			},
		},
	}

	calc := cost.NewCalculator(pricing)
	cfg := &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"anthropic-prod": {ResolvedKey: "test-key"},
		},
	}

	egress := NewEgressPipeline(map[string]provider.Adapter{"anthropic": adapter}, calc, audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), calc, cfg)

	req := &ForwardRequest{
		Envelope: []byte(`{"trace_id":"t1"}`),
		Wire: WirePayload{
			Protocol: "anthropic_messages",
			Stream:   true,
			Body:     []byte(`{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`),
		},
	}

	result, err := pipeline.Run(context.Background(), req, "test-trace-t5")
	if err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}

	if result.StatusCode != 200 {
		t.Errorf("StatusCode = %d, want 200", result.StatusCode)
	}

	if result.Body == nil {
		t.Fatal("Body is nil — expected AccountedStream")
	}

	// Verify the body is an *AccountedStream by type assertion.
	as, ok := result.Body.(*AccountedStream)
	if !ok {
		t.Fatalf("Body is %T, want *AccountedStream", result.Body)
	}

	// Drain the stream to trigger accounting.
	out, err := io.ReadAll(as)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}

	// Verify frames are forwarded unchanged.
	output := string(out)
	if !strings.Contains(output, "event: message_start") {
		t.Error("expected message_start in output")
	}
	if !strings.Contains(output, "content_block_delta") {
		t.Error("expected content_block_delta in output")
	}
	if !strings.Contains(output, "message_delta") {
		t.Error("expected message_delta in output")
	}
	if !strings.Contains(output, "message_stop") {
		t.Error("expected message_stop in output")
	}
	if !strings.Contains(output, "Hello") {
		t.Error("expected text content in output")
	}

	// Verify aicg.usage is injected at the tail with real data.
	if !strings.Contains(output, "event: aicg.usage") {
		t.Error("expected aicg.usage at stream tail")
	}

	// Verify the accounting result.
	res := as.Result()
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if res.Usage == nil {
		t.Fatal("expected usage in result")
	}
	if res.Usage.Input != 100 {
		t.Errorf("input = %d, want 100", res.Usage.Input)
	}
	if res.Usage.Output != 42 {
		t.Errorf("output = %d, want 42 (cumulative from message_delta)", res.Usage.Output)
	}
	if res.Usage.CacheRead != 50 {
		t.Errorf("cache_read = %d, want 50", res.Usage.CacheRead)
	}
	if res.Usage.CacheCreate != 20 {
		t.Errorf("cache_create = %d, want 20", res.Usage.CacheCreate)
	}
	if res.CostEvent == nil {
		t.Fatal("expected cost event")
	}
	if res.CostEvent.CostSource != "provider_usage" {
		t.Errorf("cost_source = %s, want provider_usage", res.CostEvent.CostSource)
	}
	// Cost should reflect real token counts, not zero.
	if res.CostEvent.CostCents == 0 {
		t.Errorf("CostCents = 0, want > 0 (real usage should produce non-zero cost)")
	}

	// Verify the token fields in aicg.usage match the final event.
	if !strings.Contains(output, `"input":100`) {
		t.Error("aicg.usage tokens.input should be 100")
	}
	if !strings.Contains(output, `"output":42`) {
		t.Errorf("aicg.usage tokens.output should be 42")
	}
	if !strings.Contains(output, `"cache_read":50`) {
		t.Error("aicg.usage tokens.cache_read should be 50")
	}
	if !strings.Contains(output, `"cache_create":20`) {
		t.Error("aicg.usage tokens.cache_create should be 20")
	}

	// Verify no empty-usage cost (aicg.usage cost_cents != 0 for real usage).
	if strings.Contains(output, `"cost_cents":0`) {
		t.Error("aicg.usage cost_cents should not be 0 for provider_usage path")
	}
}

// TestPipelineRunAccountedStreamOpenAI verifies the OpenAI-compatible path
// produces real usage data through AccountedStream.
func TestPipelineRunAccountedStreamOpenAI(t *testing.T) {
	sse := "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":500,\"completion_tokens\":200}}\n\ndata: [DONE]\n\n"
	adapter := &countingAdapter{name: "openai_compat", body: sse}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"openai-prod": {Wire: "openai_compat", Vendor: "openai", URL: "https://api.openai.com"},
		},
		Pools: map[string]config.Pool{
			"standard": {
				Members:     []config.PoolMember{{EndpointID: "openai-prod", Model: "gpt-4o", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, nil)

	pol, _ := policy.NewEngine(&config.PolicyConfig{
		Version: "1.0",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "route", ModelPool: "standard"},
		},
	})

	pricing := &config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor:                 "openai",
				Model:                  "gpt-4o",
				InputPricePer1KTokens:  2.5,
				OutputPricePer1KTokens: 10.0,
				Capabilities:           config.ModelCapabilities{},
			},
		},
	}

	calc := cost.NewCalculator(pricing)
	cfg := &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"openai-prod": {ResolvedKey: "test-key"},
		},
	}

	egress := NewEgressPipeline(map[string]provider.Adapter{"openai_compat": adapter}, calc, audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), calc, cfg)

	req := &ForwardRequest{
		Envelope: []byte(`{"trace_id":"t1"}`),
		Wire: WirePayload{
			Protocol: "anthropic_messages",
			Stream:   true,
			Body:     []byte(`{"model":"gpt-4o","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`),
		},
	}

	result, err := pipeline.Run(context.Background(), req, "test-trace-t5-oai")
	if err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}

	as, ok := result.Body.(*AccountedStream)
	if !ok {
		t.Fatalf("Body is %T, want *AccountedStream", result.Body)
	}

	_, _ = io.ReadAll(as)
	res := as.Result()

	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if res.Usage.Input != 500 {
		t.Errorf("input = %d, want 500", res.Usage.Input)
	}
	if res.Usage.Output != 200 {
		t.Errorf("output = %d, want 200", res.Usage.Output)
	}
	if res.CostEvent == nil {
		t.Fatal("expected cost event")
	}
	if res.CostEvent.CostCents == 0 {
		t.Errorf("CostCents = 0, want > 0")
	}
}

// TestPipelineRunMissingUsage verifies that a stream with no usage events
// produces estimated accounting with usage_missing error class.
func TestPipelineRunMissingUsage(t *testing.T) {
	sse := "data: {\"id\":\"1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"
	adapter := &countingAdapter{name: "openai_compat", body: sse}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"openai-prod": {Wire: "openai_compat", Vendor: "openai", URL: "https://api.openai.com"},
		},
		Pools: map[string]config.Pool{
			"standard": {
				Members:     []config.PoolMember{{EndpointID: "openai-prod", Model: "gpt-4o", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, nil)

	pol, _ := policy.NewEngine(&config.PolicyConfig{
		Version: "1.0",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "route", ModelPool: "standard"},
		},
	})

	pricing := &config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor:                 "openai",
				Model:                  "gpt-4o",
				InputPricePer1KTokens:  2.5,
				OutputPricePer1KTokens: 10.0,
				Capabilities:           config.ModelCapabilities{},
			},
		},
	}

	calc := cost.NewCalculator(pricing)
	cfg := &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"openai-prod": {ResolvedKey: "test-key"},
		},
	}

	egress := NewEgressPipeline(map[string]provider.Adapter{"openai_compat": adapter}, calc, audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), calc, cfg)

	req := &ForwardRequest{
		Envelope: []byte(`{"trace_id":"t1"}`),
		Wire: WirePayload{
			Protocol: "anthropic_messages",
			Stream:   true,
			Body:     []byte(`{"model":"gpt-4o","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`),
		},
	}

	result, err := pipeline.Run(context.Background(), req, "test-trace-t5-mu")
	if err != nil {
		t.Fatalf("Pipeline.Run: %v", err)
	}

	as, ok := result.Body.(*AccountedStream)
	if !ok {
		t.Fatalf("Body is %T, want *AccountedStream", result.Body)
	}

	_, _ = io.ReadAll(as)
	res := as.Result()

	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}
	if res.ErrorClass != "usage_missing" {
		t.Errorf("error_class = %q, want usage_missing", res.ErrorClass)
	}
	if res.CostEvent.CostSource != "estimated" {
		t.Errorf("cost_source = %s, want estimated", res.CostEvent.CostSource)
	}
}

// countingAdapter returns a fixed SSE body when Stream is called and counts calls.
type countingAdapter struct {
	name    string
	body    string
	streams int
}

func (a *countingAdapter) Name() string { return a.name }
func (a *countingAdapter) CountTokens(_ context.Context, _ *provider.ProviderRequest) (int, error) {
	return 0, nil
}
func (a *countingAdapter) NonStream(_ context.Context, _ *provider.ProviderRequest) ([]byte, error) {
	return nil, nil
}
func (a *countingAdapter) Stream(_ context.Context, _ *provider.ProviderRequest) (io.ReadCloser, error) {
	a.streams++
	return io.NopCloser(strings.NewReader(a.body)), nil
}
