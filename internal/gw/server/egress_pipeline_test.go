package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"agentgate/internal/gw/audit"
	"agentgate/internal/gw/config"
	"agentgate/internal/gw/cost"
	"agentgate/internal/gw/policy"
	"agentgate/internal/gw/provider"
	"agentgate/internal/gw/routing"
	"agentgate/internal/shared/ir"
)

var testUsageForDualVendor = &ir.Usage{Input: 1000, Output: 500}

func testPricingForEgress() *config.PricingConfig {
	return &config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "anthropic", Model: "claude-sonnet-4-6", Capabilities: config.ModelCapabilities{Tools: true, CacheControl: true, ExtendedThinking: true, Vision: true}},
			{Vendor: "openai", Model: "gpt-4o", Capabilities: config.ModelCapabilities{Tools: true}},
			{Vendor: "ollama-local", Model: "qwen2.5-coder:7b", Capabilities: config.ModelCapabilities{}},
		},
	}
}

func TestInjectUsageWritesSSEFrame(t *testing.T) {
	var buf strings.Builder
	routedTo := "anthropic-prod:claude-sonnet-4-6"
	usage := &ir.Usage{Input: 100, Output: 50}
	InjectUsage(&buf, "trace-001", "session-abc", usage, 42, "provider_usage", routedTo, 1, nil, 500)

	out := buf.String()
	if !strings.Contains(out, "event: aicg.usage") {
		t.Error("expected aicg.usage event")
	}
	if !strings.Contains(out, "trace-001") {
		t.Error("expected trace_id in data")
	}
	if !strings.Contains(out, "42") {
		t.Error("expected cost_cents in data")
	}
	if !strings.Contains(out, "provider_usage") {
		t.Error("expected cost_source")
	}
	if !strings.Contains(out, "anthropic-prod:claude-sonnet-4-6") {
		t.Error("expected routed_to")
	}
}

func TestInjectErrorWritesSSEFrame(t *testing.T) {
	var buf strings.Builder
	InjectError(&buf, "trace-001", "upstream_5xx", "wire returned 502", true, 318)

	out := buf.String()
	if !strings.Contains(out, "event: aicg.error") {
		t.Error("expected aicg.error event")
	}
	if !strings.Contains(out, "upstream_5xx") {
		t.Error("expected error code")
	}
	if !strings.Contains(out, "partial") {
		t.Error("expected partial flag")
	}
}

func TestBuildAICGUsageJSON(t *testing.T) {
	ce := &struct {
		Wire         string `json:"wire"`
		EndpointID   string `json:"endpoint_id"`
		Model        string `json:"model"`
		InputTokens  int    `json:"input_tokens"`
		OutputTokens int    `json:"output_tokens"`
		CostCents    int    `json:"cost_cents"`
		CostSource   string `json:"cost_source"`
		Pool         string `json:"pool"`
	}{
		Wire: "anthropic", EndpointID: "ep1", Model: "claude-sonnet-4-6",
		InputTokens: 1000, OutputTokens: 500, CostCents: 10, CostSource: "provider_usage", Pool: "standard",
	}
	_ = ce
}

func TestInjectUsageIntoResult(t *testing.T) {
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
		_ = pw.Close()
	}()

	result := &EgressResult{
		Body:      pr,
		TraceID:   "trace-001",
		AttemptNo: 1,
		Member:    routing.Member{EndpointID: "ep1", Model: "claude-sonnet-4-6"},
		Degraded:  nil,
	}

	usage := &ir.Usage{Input: 10, Output: 5}
	r := InjectUsageIntoResult(result, "session-1", usage, 3, "provider_usage", 200)
	data, _ := io.ReadAll(r)

	out := string(data)
	if !strings.Contains(out, "message_start") {
		t.Error("expected original stream content")
	}
	if !strings.Contains(out, "event: aicg.usage") {
		t.Error("expected aicg.usage at end of stream")
	}
}

func TestEgressProviderAdapterMismatch(t *testing.T) {
	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"ollama-cluster": {
				Wire:   "openai_compat",
				Vendor: "ollama-local",
				URL:    "https://ollama.internal:11434/v1",
			},
		},
		Pools: map[string]config.Pool{
			"test-pool": {
				Members: []config.PoolMember{
					{EndpointID: "ollama-cluster", Model: "qwen2.5-coder:7b", Weight: 1},
				},
				MaxAttempts: 1,
			},
		},
	}, testPricingForEgress())

	// Only "anthropic" adapter registered; pool member requires "openai_compat" → mismatch.
	adapters := map[string]provider.Adapter{"anthropic": provider.NewAnthropicAdapter()}
	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)

	reqBody := []byte(`{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	_, err := egress.Run("trace-001", "tenant-1", "user-1", "team-1", "test-pool", 1, reqBody, "", "test-key", nil)

	if err == nil {
		t.Fatal("expected error for wire-adapter mismatch, got nil")
	}
	if !strings.Contains(err.Error(), "no adapter registered for wire") {
		t.Fatalf("expected 'no adapter registered for wire' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "openai_compat") {
		t.Fatalf("expected error to mention missing wire 'openai_compat', got: %v", err)
	}
	if !strings.Contains(err.Error(), "ollama-cluster") {
		t.Fatalf("expected error to mention endpoint_id 'ollama-cluster', got: %v", err)
	}
}

func TestEgressE2EDispatchIntegration(t *testing.T) {
	var hitA atomic.Int64
	srvA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("server A (anthropic): expected /v1/messages, got %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		hitA.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srvA.Close()

	var hitB atomic.Int64
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("server B (openai): expected /chat/completions, got %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		hitB.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srvB.Close()

	adapters := map[string]provider.Adapter{
		"anthropic": provider.NewAnthropicAdapter(),
		"openai":    provider.NewOpenAICompatAdapter(),
	}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"anthropic-prod": {Wire: "anthropic", Vendor: "anthropic", URL: srvA.URL},
			"openai-prod":    {Wire: "openai", Vendor: "openai", URL: srvB.URL},
		},
		Pools: map[string]config.Pool{
			"standard": {
				Members: []config.PoolMember{
					{EndpointID: "anthropic-prod", Model: "claude-sonnet-4-6", Weight: 60},
					{EndpointID: "openai-prod", Model: "gpt-4o", Weight: 40},
				},
				MaxAttempts: 3,
			},
		},
	}, testPricingForEgress())

	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)
	reqBody := []byte(`{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)

	misrouted := 0
	for i := 0; i < 100; i++ {
		aBefore := hitA.Load()
		bBefore := hitB.Load()
		traceID := fmt.Sprintf("trace-dispatch-e2e-%03d", i)

		result, err := egress.Run(traceID, "tenant-1", "user-1", "team-1", "standard", 1, reqBody, "", "test-key", nil)
		if err != nil {
			t.Fatalf("Run[%d]: unexpected error: %v", i, err)
		}
		_ = result.Body.Close()

		aAfter := hitA.Load()
		bAfter := hitB.Load()

		aDelta := aAfter - aBefore
		bDelta := bAfter - bBefore
		totalHits := aDelta + bDelta
		if totalHits != 1 {
			t.Errorf("Run[%d]: expected 1 upstream hit, got %d (aDelta=%d bDelta=%d)", i, totalHits, aDelta, bDelta)
		}

		switch result.Member.Wire {
		case "anthropic":
			if aDelta != 1 {
				misrouted++
				t.Errorf("Run[%d]: routed to anthropic but server A delta is %d", i, aDelta)
			}
		case "openai":
			if bDelta != 1 {
				misrouted++
				t.Errorf("Run[%d]: routed to openai but server B delta is %d", i, bDelta)
			}
		default:
			t.Errorf("Run[%d]: unexpected wire %q", i, result.Member.Wire)
		}
	}

	finalA := hitA.Load()
	finalB := hitB.Load()
	if misrouted > 0 {
		t.Errorf("dispatch misrouted: %d/%d requests hit wrong upstream", misrouted, 100)
	}
	if finalA == 0 {
		t.Error("server A (anthropic) was never hit")
	}
	if finalB == 0 {
		t.Error("server B (openai) was never hit")
	}
	if finalA < 10 {
		t.Errorf("server A hit %d times (< 10), weight distribution may be off", finalA)
	}
	if finalB < 10 {
		t.Errorf("server B hit %d times (< 10), weight distribution may be off", finalB)
	}
	t.Logf("dispatch distribution: anthropic=%d openai=%d, misrouted=%d", finalA, finalB, misrouted)
}

type mockAdapter struct {
	name    string
	streams []string
}

func (a *mockAdapter) Name() string { return a.name }
func (a *mockAdapter) CountTokens(_ context.Context, _ *provider.ProviderRequest) (int, error) {
	return 0, nil
}
func (a *mockAdapter) NonStream(_ context.Context, _ *provider.ProviderRequest) ([]byte, error) {
	return nil, nil
}
func (a *mockAdapter) Stream(_ context.Context, req *provider.ProviderRequest) (io.ReadCloser, error) {
	a.streams = append(a.streams, req.Model)
	return io.NopCloser(strings.NewReader("data: ok\n\n")), nil
}

func TestEgressAdapterDispatchMultiProvider(t *testing.T) {
	anthropicAdapter := &mockAdapter{name: "anthropic"}
	openAIAdapter := &mockAdapter{name: "openai_compat"}

	adapters := map[string]provider.Adapter{
		"anthropic":     anthropicAdapter,
		"openai_compat": openAIAdapter,
	}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"anthropic-prod": {Wire: "anthropic", Vendor: "anthropic", URL: "https://api.anthropic.com"},
			"openai-prod":    {Wire: "openai_compat", Vendor: "openai", URL: "https://api.openai.com/v1"},
		},
		Pools: map[string]config.Pool{
			"test-pool": {
				Members: []config.PoolMember{
					{EndpointID: "anthropic-prod", Model: "claude-sonnet-4-6", Weight: 60},
					{EndpointID: "openai-prod", Model: "gpt-4o", Weight: 40},
				},
				MaxAttempts: 3,
			},
		},
	}, testPricingForEgress())

	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)
	reqBody := []byte(`{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)

	anthropicHits := 0
	openAIHits := 0
	for i := 0; i < 100; i++ {
		traceID := fmt.Sprintf("trace-dispatch-%03d", i)
		anthropicAdapter.streams = nil
		openAIAdapter.streams = nil

		result, err := egress.Run(traceID, "tenant-1", "user-1", "team-1", "test-pool", 1, reqBody, "", "test-key", nil)
		if err != nil {
			t.Fatalf("Run[%d]: unexpected error: %v", i, err)
		}
		_ = result.Body.Close()

		switch result.Member.Wire {
		case "anthropic":
			anthropicHits++
			if len(anthropicAdapter.streams) != 1 {
				t.Errorf("Run[%d]: anthropic adapter should have 1 stream, got %d", i, len(anthropicAdapter.streams))
			}
			if len(openAIAdapter.streams) != 0 {
				t.Errorf("Run[%d]: openai_compat adapter should have 0 streams when routing to anthropic, got %d", i, len(openAIAdapter.streams))
			}
		case "openai_compat":
			openAIHits++
			if len(openAIAdapter.streams) != 1 {
				t.Errorf("Run[%d]: openai_compat adapter should have 1 stream, got %d", i, len(openAIAdapter.streams))
			}
			if len(anthropicAdapter.streams) != 0 {
				t.Errorf("Run[%d]: anthropic adapter should have 0 streams when routing to openai_compat, got %d", i, len(anthropicAdapter.streams))
			}
		default:
			t.Errorf("Run[%d]: unexpected wire %q", i, result.Member.Wire)
		}
	}

	if anthropicHits == 0 {
		t.Error("expected at least one dispatch to anthropic adapter")
	}
	if openAIHits == 0 {
		t.Error("expected at least one dispatch to openai_compat adapter")
	}
	t.Logf("dispatch distribution: anthropic=%d openai_compat=%d", anthropicHits, openAIHits)
}

func TestEgressAdapterNotFound(t *testing.T) {
	adapters := map[string]provider.Adapter{
		"anthropic": &mockAdapter{name: "anthropic"},
	}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"ollama-cluster": {Wire: "openai_compat", Vendor: "ollama-local", URL: "https://ollama.internal:11434/v1"},
		},
		Pools: map[string]config.Pool{
			"test-pool": {
				Members:     []config.PoolMember{{EndpointID: "ollama-cluster", Model: "qwen2.5-coder:7b", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, testPricingForEgress())

	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)
	reqBody := []byte(`{"model":"qwen2.5-coder:7b","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`)
	_, err := egress.Run("trace-001", "tenant-1", "user-1", "team-1", "test-pool", 1, reqBody, "", "test-key", nil)

	if err == nil {
		t.Fatal("expected error for missing adapter, got nil")
	}
	if !strings.Contains(err.Error(), "no adapter registered for wire") {
		t.Fatalf("expected 'no adapter registered for wire' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), `"openai_compat"`) {
		t.Fatalf("expected error to mention wire 'openai_compat', got: %v", err)
	}
	if !strings.Contains(err.Error(), "ollama-cluster") {
		t.Fatalf("expected error to mention endpoint 'ollama-cluster', got: %v", err)
	}
}

func TestGLMDualVendorRouting(t *testing.T) {
	bigmodelAdapter := &mockAdapter{name: "anthropic_compat"}
	bailianAdapter := &mockAdapter{name: "openai_compat"}

	adapters := map[string]provider.Adapter{
		"anthropic_compat": bigmodelAdapter,
		"openai_compat":    bailianAdapter,
	}

	registry := map[string]config.ProviderEndpoint{
		"glm-bigmodel": {Wire: "anthropic_compat", Vendor: "bigmodel-direct", URL: "https://open.bigmodel.cn/api/anthropic", TrustTier: "vendor", Supports: config.EndpointSupports{Streaming: true}},
		"glm-bailian":  {Wire: "openai_compat", Vendor: "bailian", URL: "https://bailian.aliyun.com/api", TrustTier: "vendor", Supports: config.EndpointSupports{Streaming: true}},
	}

	pricing := &config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "bigmodel-direct", Model: "glm-4.7", Currency: "CNY", InputPricePer1KTokens: 1.0, OutputPricePer1KTokens: 2.0, Capabilities: config.ModelCapabilities{CacheControl: false}},
			{Vendor: "bailian", Model: "glm-4.7", Currency: "CNY", InputPricePer1KTokens: 3.0, OutputPricePer1KTokens: 6.0, Capabilities: config.ModelCapabilities{CacheControl: true}},
		},
	}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: registry,
		Pools: map[string]config.Pool{
			"cn-glm": {
				Members: []config.PoolMember{
					{EndpointID: "glm-bigmodel", Model: "glm-4.7", Weight: 50},
					{EndpointID: "glm-bailian", Model: "glm-4.7", Weight: 50},
				},
				MaxAttempts: 2,
				TimeoutMs:   90000,
			},
		},
	}, pricing)

	calc := cost.NewCalculator(pricing)
	egress := NewEgressPipeline(adapters, calc, audit.NewWriter(), sel)
	reqBody := []byte(`{"model":"glm-4.7","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"Hello"}]}]}`)

	// Assertion 1: cache_control constraint selects only Bailian (cache_control:true).
	chain, err := sel.BuildChain("cn-glm", &policy.Decision{
		PrimaryAction:        policy.ActionRoute,
		RequiredCapabilities: []string{"cache_control"},
	}, "trace-cc-001", 1)
	if err != nil {
		t.Fatalf("BuildChain with cache_control: %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("Assertion 1: expected 1 candidate with cache_control, got %d", len(chain))
	}
	if chain[0].EndpointID != "glm-bailian" {
		t.Errorf("Assertion 1: expected Bailian (cache_control:true), got %s", chain[0].EndpointID)
	}
	if chain[0].VendorID != "bailian" {
		t.Errorf("Assertion 1: vendor_id = %q, want bailian", chain[0].VendorID)
	}

	// Assertion 2: 100 unconstrained requests with distinct trace IDs show
	// approximately weighted distribution (each vendor 30–70) and correct
	// cost_cents matching that vendor's pricing row.
	bigmodelHits := 0
	bailianHits := 0
	for i := 0; i < 100; i++ {
		traceID := fmt.Sprintf("trace-glm-dual-%03d", i)
		bigmodelAdapter.streams = nil
		bailianAdapter.streams = nil

		result, err := egress.Run(traceID, "tenant-1", "user-1", "team-1", "cn-glm", 1, reqBody, "", "test-key", nil)
		if err != nil {
			t.Fatalf("Run[%d]: %v", i, err)
		}
		_ = result.Body.Close()

		m := result.Member
		switch m.VendorID {
		case "bigmodel-direct":
			bigmodelHits++
			if m.Wire != "anthropic_compat" {
				t.Errorf("Run[%d]: bigmodel-direct wire = %q, want anthropic_compat", i, m.Wire)
			}
			// bigmodel-direct: 1.0 input, 2.0 output CNY/1k; 1000 in + 500 out = 1.0 + 1.0 = 2 fen
			expectedCents, _, _, _, _, expectedCur, _ := calc.Calculate("bigmodel-direct", "glm-4.7", 1000, 500, 0, 0)
			if expectedCents != 2 || expectedCur != "CNY" {
				t.Errorf("Run[%d]: expected bigmodel-direct cost 2 CNY, got %d %s", i, expectedCents, expectedCur)
			}
			ce := cost.BuildEvent(testUsageForDualVendor, m.Wire, m.VendorID, m.EndpointID, m.Model, "cn-glm", m.Private, calc)
			if ce.VendorID != "bigmodel-direct" {
				t.Errorf("Run[%d]: cost event vendor_id = %q, want bigmodel-direct", i, ce.VendorID)
			}
			if ce.CostCents != 2 {
				t.Errorf("Run[%d]: bigmodel-direct cost_cents = %d, want 2", i, ce.CostCents)
			}
			if ce.Currency != "CNY" {
				t.Errorf("Run[%d]: currency = %q, want CNY", i, ce.Currency)
			}
		case "bailian":
			bailianHits++
			if m.Wire != "openai_compat" {
				t.Errorf("Run[%d]: bailian wire = %q, want openai_compat", i, m.Wire)
			}
			// bailian: 3.0 input, 6.0 output CNY/1k; 1000 in + 500 out = 3.0 + 3.0 = 6 fen
			expectedCents, _, _, _, _, expectedCur, _ := calc.Calculate("bailian", "glm-4.7", 1000, 500, 0, 0)
			if expectedCents != 6 || expectedCur != "CNY" {
				t.Errorf("Run[%d]: expected bailian cost 6 CNY, got %d %s", i, expectedCents, expectedCur)
			}
			ce := cost.BuildEvent(testUsageForDualVendor, m.Wire, m.VendorID, m.EndpointID, m.Model, "cn-glm", m.Private, calc)
			if ce.VendorID != "bailian" {
				t.Errorf("Run[%d]: cost event vendor_id = %q, want bailian", i, ce.VendorID)
			}
			if ce.CostCents != 6 {
				t.Errorf("Run[%d]: bailian cost_cents = %d, want 6", i, ce.CostCents)
			}
		default:
			t.Errorf("Run[%d]: unexpected vendor_id %q", i, m.VendorID)
		}
	}

	if bigmodelHits < 30 || bigmodelHits > 70 {
		t.Errorf("Assertion 2: bigmodel-direct hits %d outside acceptable range [30,70] for 50/50 weight over 100 requests", bigmodelHits)
	}
	if bailianHits < 30 || bailianHits > 70 {
		t.Errorf("Assertion 2: bailian hits %d outside acceptable range [30,70] for 50/50 weight over 100 requests", bailianHits)
	}
	t.Logf("dual-vendor distribution: bigmodel-direct=%d bailian=%d", bigmodelHits, bailianHits)
}

type recordingAdapter struct {
	name       string
	messages   []byte
	system     []byte
	tools      []byte
	toolChoice []byte
	metadata   map[string]string
	thinking   []byte
}

func (a *recordingAdapter) Name() string { return a.name }
func (a *recordingAdapter) CountTokens(_ context.Context, _ *provider.ProviderRequest) (int, error) {
	return 0, nil
}
func (a *recordingAdapter) NonStream(_ context.Context, _ *provider.ProviderRequest) ([]byte, error) {
	return nil, nil
}
func (a *recordingAdapter) Stream(_ context.Context, req *provider.ProviderRequest) (io.ReadCloser, error) {
	a.messages = req.Messages
	a.system = req.System
	a.tools = req.Tools
	a.toolChoice = req.ToolChoice
	a.metadata = req.Metadata
	a.thinking = req.Thinking
	return io.NopCloser(strings.NewReader("data: ok\n\n")), nil
}

func TestEgressSurfacesAnthropicParseError(t *testing.T) {
	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"anthropic-prod": {Wire: "anthropic", Vendor: "anthropic", URL: "https://api.anthropic.com"},
		},
		Pools: map[string]config.Pool{
			"test-pool": {
				Members:     []config.PoolMember{{EndpointID: "anthropic-prod", Model: "claude-sonnet-4-6", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, testPricingForEgress())

	adapters := map[string]provider.Adapter{
		"anthropic": &recordingAdapter{name: "anthropic"},
	}
	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)

	// Malformed body: content is a number, not string or array.
	reqBody := []byte(`{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":123}]}`)
	_, err := egress.Run("trace-001", "tenant-1", "user-1", "team-1", "test-pool", 1, reqBody, "", "test-key", nil)

	if err == nil {
		t.Fatal("expected error for malformed request, got nil")
	}
	if !strings.Contains(err.Error(), "parse anthropic request") {
		t.Errorf("error should contain 'parse anthropic request', got: %v", err)
	}
	// The underlying cause should be an UnmarshalTypeError (or similar json error).
	var utErr *json.UnmarshalTypeError
	if !errors.As(err, &utErr) {
		// Fallback: check for any json error string.
		if !strings.Contains(err.Error(), "json:") && !strings.Contains(err.Error(), "cannot unmarshal") {
			t.Errorf("error should wrap a json parse error, got: %v", err)
		}
	}
}

func TestEgressAnthropicWireUsesIRToAnthropic(t *testing.T) {
	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"anthropic-prod": {Wire: "anthropic", Vendor: "anthropic", URL: "https://api.anthropic.com"},
		},
		Pools: map[string]config.Pool{
			"test-pool": {
				Members:     []config.PoolMember{{EndpointID: "anthropic-prod", Model: "claude-sonnet-4-6", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, testPricingForEgress())

	rec := &recordingAdapter{name: "anthropic"}
	adapters := map[string]provider.Adapter{"anthropic": rec}
	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)

	reqBody := []byte(`{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}`)
	result, err := egress.Run("trace-001", "tenant-1", "user-1", "team-1", "test-pool", 1, reqBody, "", "test-key", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = result.Body.Close()

	if rec.messages == nil {
		t.Fatal("adapter.Messages was not set")
	}

	// req.Messages should be a JSON array of message objects.
	var msgs []json.RawMessage
	if err := json.Unmarshal(rec.messages, &msgs); err != nil {
		t.Fatalf("req.Messages is not a JSON array: %v (got: %s)", err, string(rec.messages))
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	var parsed struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msgs[0], &parsed); err != nil {
		t.Fatalf("parse message: %v", err)
	}
	if parsed.Role != "user" {
		t.Errorf("role = %q, want user", parsed.Role)
	}
	if len(parsed.Content) != 1 || parsed.Content[0].Type != "text" || parsed.Content[0].Text != "hello" {
		t.Errorf("content = %+v, want [{type:text text:hello}]", parsed.Content)
	}
}

func TestEgressAnthropicCompatWireUsesIRToAnthropic(t *testing.T) {
	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"glm-bigmodel": {Wire: "anthropic_compat", Vendor: "bigmodel-direct", URL: "https://open.bigmodel.cn/api/anthropic"},
		},
		Pools: map[string]config.Pool{
			"test-pool": {
				Members:     []config.PoolMember{{EndpointID: "glm-bigmodel", Model: "glm-4.7", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, testPricingForEgress())

	rec := &recordingAdapter{name: "anthropic_compat"}
	adapters := map[string]provider.Adapter{"anthropic_compat": rec}
	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)

	reqBody := []byte(`{"model":"glm-4.7","max_tokens":100,"messages":[{"role":"user","content":"string shorthand"}]}`)
	result, err := egress.Run("trace-001", "tenant-1", "user-1", "team-1", "test-pool", 1, reqBody, "", "test-key", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = result.Body.Close()

	var msgs []json.RawMessage
	if err := json.Unmarshal(rec.messages, &msgs); err != nil {
		t.Fatalf("req.Messages is not a JSON array: %v (got: %s)", err, string(rec.messages))
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	// String-shorthand content must be normalised to array-of-blocks.
	var parsed struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msgs[0], &parsed); err != nil {
		t.Fatalf("parse message: %v", err)
	}
	if len(parsed.Content) != 1 || parsed.Content[0].Type != "text" || parsed.Content[0].Text != "string shorthand" {
		t.Errorf("content = %+v, want [{type:text text:string shorthand}]", parsed.Content)
	}
}

func TestEgressDegradationStripsFeaturesForCompatWire(t *testing.T) {
	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"glm-bigmodel": {Wire: "anthropic_compat", Vendor: "bigmodel-direct", URL: "https://open.bigmodel.cn/api/anthropic"},
		},
		Pools: map[string]config.Pool{
			"test-pool": {
				Members:     []config.PoolMember{{EndpointID: "glm-bigmodel", Model: "glm-4.7", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, testPricingForEgress())

	rec := &recordingAdapter{name: "anthropic_compat"}
	adapters := map[string]provider.Adapter{"anthropic_compat": rec}
	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)

	reqBody := []byte(`{"model":"glm-4.7","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":4000},"messages":[{"role":"user","content":[{"type":"text","text":"Hello","cache_control":{"type":"ephemeral"}}]}]}`)
	result, err := egress.Run("trace-001", "tenant-1", "user-1", "team-1", "test-pool", 1, reqBody, "", "test-key", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = result.Body.Close()

	// Verify degradation recorded.
	if len(result.Degraded) != 2 {
		t.Fatalf("expected 2 degraded features, got %d: %v", len(result.Degraded), result.Degraded)
	}
	hasCC := false
	hasET := false
	for _, d := range result.Degraded {
		if d == "cache_control" {
			hasCC = true
		}
		if d == "extended_thinking" {
			hasET = true
		}
	}
	if !hasCC {
		t.Error("expected cache_control in degraded features")
	}
	if !hasET {
		t.Error("expected extended_thinking in degraded features")
	}

	// Verify that the message sent to upstream does NOT contain cache_control.
	var msgs []json.RawMessage
	if err := json.Unmarshal(rec.messages, &msgs); err != nil {
		t.Fatalf("parse messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	var parsed struct {
		Role    string `json:"role"`
		Content []struct {
			Type         string          `json:"type"`
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msgs[0], &parsed); err != nil {
		t.Fatalf("parse message: %v", err)
	}
	for _, c := range parsed.Content {
		if len(c.CacheControl) > 0 {
			t.Error("cache_control should have been stripped from content block")
		}
	}
}

func TestEgressDegradationPreservesFeaturesForAnthropicWire(t *testing.T) {
	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"anthropic-prod": {Wire: "anthropic", Vendor: "anthropic", URL: "https://api.anthropic.com"},
		},
		Pools: map[string]config.Pool{
			"test-pool": {
				Members:     []config.PoolMember{{EndpointID: "anthropic-prod", Model: "claude-sonnet-4-6", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, testPricingForEgress())

	rec := &recordingAdapter{name: "anthropic"}
	adapters := map[string]provider.Adapter{"anthropic": rec}
	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)

	reqBody := []byte(`{"model":"claude-sonnet-4-6","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":4000},"messages":[{"role":"user","content":[{"type":"text","text":"Hello","cache_control":{"type":"ephemeral"}}]}]}`)
	result, err := egress.Run("trace-001", "tenant-1", "user-1", "team-1", "test-pool", 1, reqBody, "", "test-key", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = result.Body.Close()

	// Anthropic wire must not degrade.
	if len(result.Degraded) != 0 {
		t.Errorf("expected no degraded features for anthropic wire, got %v", result.Degraded)
	}

	// Verify that the message sent to upstream still contains cache_control.
	var msgs []json.RawMessage
	if err := json.Unmarshal(rec.messages, &msgs); err != nil {
		t.Fatalf("parse messages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	var parsed struct {
		Role    string `json:"role"`
		Content []struct {
			Type         string          `json:"type"`
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control,omitempty"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msgs[0], &parsed); err != nil {
		t.Fatalf("parse message: %v", err)
	}
	foundCacheControl := false
	for _, c := range parsed.Content {
		if len(c.CacheControl) > 0 {
			foundCacheControl = true
			break
		}
	}
	if !foundCacheControl {
		t.Error("cache_control should be preserved for anthropic wire")
	}
}

func TestEgressUnsupportedWire(t *testing.T) {
	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"custom": {Wire: "custom_wire", Vendor: "custom", URL: "https://custom.example.com"},
		},
		Pools: map[string]config.Pool{
			"test-pool": {
				Members:     []config.PoolMember{{EndpointID: "custom", Model: "custom-model", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, testPricingForEgress())

	adapters := map[string]provider.Adapter{
		"custom_wire": &recordingAdapter{name: "custom_wire"},
	}
	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)

	reqBody := []byte(`{"model":"custom-model","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	_, err := egress.Run("trace-001", "tenant-1", "user-1", "team-1", "test-pool", 1, reqBody, "", "test-key", nil)

	if err == nil {
		t.Fatal("expected error for unsupported wire, got nil")
	}
	if !strings.Contains(err.Error(), "unsupported wire") {
		t.Errorf("error should contain 'unsupported wire', got: %v", err)
	}
	if !strings.Contains(err.Error(), "custom_wire") {
		t.Errorf("error should contain wire name, got: %v", err)
	}
}

func TestEgressAnthropicWireForwardsAllTopLevelFields(t *testing.T) {
	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"anthropic-prod": {Wire: "anthropic", Vendor: "anthropic", URL: "https://api.anthropic.com"},
		},
		Pools: map[string]config.Pool{
			"test-pool": {
				Members:     []config.PoolMember{{EndpointID: "anthropic-prod", Model: "claude-sonnet-4-6", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, testPricingForEgress())

	rec := &recordingAdapter{name: "anthropic"}
	adapters := map[string]provider.Adapter{"anthropic": rec}
	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)

	reqBody := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"system": [{"type": "text", "text": "You are a helpful assistant"}],
		"tools": [{"name": "write_file", "description": "Write a file", "input_schema": {"type": "object"}}],
		"tool_choice": {"type": "auto"},
		"metadata": {"user_id": "user-123"},
		"thinking": {"type": "enabled", "budget_tokens": 16000},
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Write a file"}]}]
	}`)
	result, err := egress.Run("trace-001", "tenant-1", "user-1", "team-1", "test-pool", 1, reqBody, "", "test-key", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = result.Body.Close()

	if len(rec.system) == 0 {
		t.Error("system not forwarded to adapter")
	}
	if len(rec.tools) == 0 {
		t.Error("tools not forwarded to adapter")
	}
	if len(rec.toolChoice) == 0 {
		t.Error("tool_choice not forwarded to adapter")
	}
	if rec.metadata == nil || rec.metadata["user_id"] != "user-123" {
		t.Errorf("metadata not forwarded: %v", rec.metadata)
	}
	if len(rec.thinking) == 0 {
		t.Error("thinking not forwarded to adapter")
	}
	if len(result.Degraded) != 0 {
		t.Errorf("expected no degraded features for anthropic wire, got %v", result.Degraded)
	}
}

func TestEgressAnthropicCompatWireStripsThinkingPreservesTools(t *testing.T) {
	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"glm-bigmodel": {Wire: "anthropic_compat", Vendor: "bigmodel-direct", URL: "https://open.bigmodel.cn/api/anthropic"},
		},
		Pools: map[string]config.Pool{
			"test-pool": {
				Members:     []config.PoolMember{{EndpointID: "glm-bigmodel", Model: "glm-4.7", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, testPricingForEgress())

	rec := &recordingAdapter{name: "anthropic_compat"}
	adapters := map[string]provider.Adapter{"anthropic_compat": rec}
	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)

	reqBody := []byte(`{
		"model": "glm-4.7",
		"max_tokens": 1024,
		"stream": true,
		"tools": [{"name": "read_file", "description": "Read a file", "input_schema": {"type": "object"}}],
		"tool_choice": {"type": "any"},
		"thinking": {"type": "enabled", "budget_tokens": 4000},
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Hello", "cache_control": {"type": "ephemeral"}}]}]
	}`)
	result, err := egress.Run("trace-001", "tenant-1", "user-1", "team-1", "test-pool", 1, reqBody, "", "test-key", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = result.Body.Close()

	if len(rec.tools) == 0 {
		t.Error("tools not forwarded to anthropic_compat adapter")
	}
	if len(rec.toolChoice) == 0 {
		t.Error("tool_choice not forwarded to anthropic_compat adapter")
	}
	if len(rec.thinking) != 0 {
		t.Error("thinking should be stripped for anthropic_compat wire but was forwarded")
	}
	foundThinking := false
	for _, d := range result.Degraded {
		if d == "extended_thinking" {
			foundThinking = true
		}
	}
	if !foundThinking {
		t.Error("expected extended_thinking in degraded features")
	}
	var msgs []json.RawMessage
	_ = json.Unmarshal(rec.messages, &msgs)
	if len(msgs) > 0 {
		var parsed struct {
			Content []struct {
				CacheControl json.RawMessage `json:"cache_control"`
			} `json:"content"`
		}
		_ = json.Unmarshal(msgs[0], &parsed)
		for _, c := range parsed.Content {
			if len(c.CacheControl) > 0 {
				t.Error("cache_control should be stripped from content block")
			}
		}
	}
}

// TestEgressOpenAIWireIncludesStreamOptions verifies that OpenAI-compatible
// streaming requests include stream_options.include_usage=true in the body
// sent to the upstream adapter (T4 DoD #3).
func TestEgressOpenAIWireIncludesStreamOptions(t *testing.T) {
	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"openai-prod": {Wire: "openai", Vendor: "openai", URL: "https://api.openai.com/v1"},
		},
		Pools: map[string]config.Pool{
			"test-pool": {
				Members:     []config.PoolMember{{EndpointID: "openai-prod", Model: "gpt-4o", Weight: 1}},
				MaxAttempts: 1,
			},
		},
	}, testPricingForEgress())

	rec := &recordingAdapter{name: "openai"}
	adapters := map[string]provider.Adapter{"openai": rec}
	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)

	reqBody := []byte(`{"model":"gpt-4o","max_tokens":500,"stream":true,"messages":[{"role":"user","content":"Hello"}]}`)
	result, err := egress.Run("trace-001", "tenant-1", "user-1", "team-1", "test-pool", 1, reqBody, "", "test-key", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	_ = result.Body.Close()

	if rec.messages == nil {
		t.Fatal("adapter.Messages was not set")
	}

	var parsed struct {
		Stream        bool `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options,omitempty"`
	}
	if err := json.Unmarshal(rec.messages, &parsed); err != nil {
		t.Fatalf("unmarshal adapter messages: %v (body: %s)", err, string(rec.messages))
	}
	if !parsed.Stream {
		t.Error("expected stream=true")
	}
	if parsed.StreamOptions == nil {
		t.Fatal("expected stream_options in request body")
	}
	if !parsed.StreamOptions.IncludeUsage {
		t.Error("expected stream_options.include_usage=true")
	}
}
