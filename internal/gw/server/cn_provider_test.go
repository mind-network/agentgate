package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"agentgate/internal/gw/audit"
	"agentgate/internal/gw/config"
	"agentgate/internal/gw/cost"
	"agentgate/internal/gw/policy"
	"agentgate/internal/gw/provider"
	"agentgate/internal/gw/routing"
)

func TestCNProviderHappyPath(t *testing.T) {
	var gotAPIKey, gotAnthropicVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("expected /v1/messages, got %s", r.URL.Path)
		}
		gotAPIKey = r.Header.Get("x-api-key")
		gotAnthropicVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	pricing := &config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "deepseek-direct", Model: "deepseek-v4-pro", Currency: "CNY", InputPricePer1KTokens: 2.0, OutputPricePer1KTokens: 8.0},
		},
	}
	pricing.DefaultCurrencies()

	calc := cost.NewCalculator(pricing)

	adapters := map[string]provider.Adapter{
		"anthropic_compat": provider.NewAnthropicCompatAdapter(),
	}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"deepseek-anthropic": {
				Wire:   "anthropic_compat",
				Vendor: "deepseek-direct",
				URL:    srv.URL,
				Supports: config.EndpointSupports{
					Streaming: true,
				},
			},
		},
		Pools: map[string]config.Pool{
			"cn-default": {
				Members:     []config.PoolMember{{EndpointID: "deepseek-anthropic", Model: "deepseek-v4-pro", Weight: 100}},
				MaxAttempts: 1,
			},
		},
	}, pricing)

	egress := NewEgressPipeline(adapters, calc, audit.NewWriter(), sel)
	reqBody := []byte(`{"model":"deepseek-v4-pro","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"Hello"}]}]}`)

	result, err := egress.Run("trace-cn-001", "tenant-1", "user-1", "team-1", "cn-default", 1, reqBody, "", "test-api-key", nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	defer func() { _ = result.Body.Close() }()

	if result.Member.Wire != "anthropic_compat" {
		t.Errorf("wire = %q, want anthropic_compat", result.Member.Wire)
	}
	if result.Member.VendorID != "deepseek-direct" {
		t.Errorf("vendor_id = %q, want deepseek-direct", result.Member.VendorID)
	}
	if result.Member.EndpointID != "deepseek-anthropic" {
		t.Errorf("endpoint_id = %q, want deepseek-anthropic", result.Member.EndpointID)
	}
	if result.Member.Model != "deepseek-v4-pro" {
		t.Errorf("model = %q, want deepseek-v4-pro", result.Member.Model)
	}

	if gotAPIKey != "test-api-key" {
		t.Errorf("upstream got x-api-key = %q, want test-api-key", gotAPIKey)
	}
	if gotAnthropicVersion != "2023-06-01" {
		t.Errorf("upstream got anthropic-version = %q, want 2023-06-01", gotAnthropicVersion)
	}

	ce := cost.BuildEvent(nil, result.Member.Wire, result.Member.VendorID, result.Member.EndpointID, result.Member.Model, "cn-default", false, calc)
	if ce.Currency != "CNY" {
		t.Errorf("cost event currency = %q, want CNY", ce.Currency)
	}
	if ce.Wire != "anthropic_compat" {
		t.Errorf("cost event wire = %q, want anthropic_compat", ce.Wire)
	}
	if ce.VendorID != "deepseek-direct" {
		t.Errorf("cost event vendor_id = %q, want deepseek-direct", ce.VendorID)
	}
	if ce.Model != "deepseek-v4-pro" {
		t.Errorf("cost event model = %q, want deepseek-v4-pro", ce.Model)
	}
}

func TestCNProviderCapabilityRejectIngress(t *testing.T) {
	var upstreamHit atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit.Add(1)
		t.Error("upstream must not be called for capability-rejected request")
		w.WriteHeader(500)
	}))
	defer srv.Close()

	pol, _ := policy.NewEngine(&config.PolicyConfig{
		Version: "1.0",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "route", ModelPool: "cn-default"},
		},
	})

	pricing := &config.PricingConfig{
		Models: []config.ModelPricing{
			{Vendor: "deepseek-direct", Model: "deepseek-v4-pro", Capabilities: config.ModelCapabilities{CacheControl: false}},
		},
	}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"deepseek-anthropic": {
				Wire:   "anthropic_compat",
				Vendor: "deepseek-direct",
				URL:    srv.URL,
				Supports: config.EndpointSupports{
					Streaming: true,
				},
			},
		},
		Pools: map[string]config.Pool{
			"cn-default": {
				Members:     []config.PoolMember{{EndpointID: "deepseek-anthropic", Model: "deepseek-v4-pro", Weight: 100}},
				MaxAttempts: 1,
			},
		},
	}, pricing)

	adapters := map[string]provider.Adapter{
		"anthropic_compat": provider.NewAnthropicCompatAdapter(),
	}

	cfg := &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"deepseek-anthropic": {ResolvedKey: "test-key"},
		},
	}

	egress := NewEgressPipeline(adapters, cost.NewCalculator(pricing), audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), cost.NewCalculator(pricing), cfg)
	handler := NewHandler(pipeline, nil, nil)

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Post("/v1/agent/forward", handler.HandleForward)

	reqBody := `{
		"envelope": {"trace_id": "t1"},
		"wire": {
			"protocol": "anthropic_messages",
			"stream": true,
			"body": {
				"model": "deepseek-v4-pro",
				"max_tokens": 100,
				"messages": [
					{
						"role": "user",
						"content": [
							{"type": "text", "text": "Hello", "cache_control": {"type": "ephemeral"}}
						]
					}
				]
			}
		}
	}`

	req := httptest.NewRequest("POST", "/v1/agent/forward", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for capability reject, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("parse error response: %v", err)
	}
	if errResp.Code != "no_eligible_endpoint" {
		t.Errorf("error code = %q, want no_eligible_endpoint", errResp.Code)
	}
	if !strings.Contains(errResp.Message, "no candidate") {
		t.Errorf("error message should indicate no candidate, got: %q", errResp.Message)
	}
	if !strings.Contains(errResp.Message, "cache_control") {
		t.Errorf("error message should mention cache_control constraint, got: %q", errResp.Message)
	}

	if upstreamHit.Load() != 0 {
		t.Errorf("upstream received %d requests, want 0 (selector should reject before dispatch)", upstreamHit.Load())
	}
}

func TestCNProviderUpstreamError4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()

	adapters := map[string]provider.Adapter{
		"anthropic_compat": provider.NewAnthropicCompatAdapter(),
	}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"deepseek-anthropic": {
				Wire:   "anthropic_compat",
				Vendor: "deepseek-direct",
				URL:    srv.URL,
				Supports: config.EndpointSupports{
					Streaming: true,
				},
			},
		},
		Pools: map[string]config.Pool{
			"cn-default": {
				Members:     []config.PoolMember{{EndpointID: "deepseek-anthropic", Model: "deepseek-v4-pro", Weight: 100}},
				MaxAttempts: 1,
			},
		},
	}, nil)

	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)
	reqBody := []byte(`{"model":"deepseek-v4-pro","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"Hello"}]}]}`)

	_, err := egress.Run("trace-cn-401", "tenant-1", "user-1", "team-1", "cn-default", 1, reqBody, "", "bad-key", nil)
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}

	var upErr *provider.UpstreamError
	if !errors.As(err, &upErr) {
		t.Errorf("expected *provider.UpstreamError, got %T: %v", err, err)
	}
	if upErr.Status != 401 {
		t.Errorf("upstream error status = %d, want 401", upErr.Status)
	}
}

func TestCNProviderStandardPoolUnaffected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	adapters := map[string]provider.Adapter{
		"anthropic":        provider.NewAnthropicAdapter(),
		"anthropic_compat": provider.NewAnthropicCompatAdapter(),
	}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"anthropic-prod": {
				Wire:   "anthropic",
				Vendor: "anthropic",
				URL:    srv.URL,
				Supports: config.EndpointSupports{
					Streaming: true,
				},
			},
			"deepseek-anthropic": {
				Wire:   "anthropic_compat",
				Vendor: "deepseek-direct",
				URL:    "https://api.deepseek.com/anthropic",
				Supports: config.EndpointSupports{
					Streaming: true,
				},
			},
		},
		Pools: map[string]config.Pool{
			"standard": {
				Members:     []config.PoolMember{{EndpointID: "anthropic-prod", Model: "claude-sonnet-4-6", Weight: 100}},
				MaxAttempts: 1,
			},
			"cn-default": {
				Members:     []config.PoolMember{{EndpointID: "deepseek-anthropic", Model: "deepseek-v4-pro", Weight: 100}},
				MaxAttempts: 1,
			},
		},
	}, nil)

	egress := NewEgressPipeline(adapters, nil, audit.NewWriter(), sel)
	reqBody := []byte(`{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"Hello"}]}]}`)

	result, err := egress.Run("trace-std-001", "tenant-1", "user-1", "team-1", "standard", 1, reqBody, "", "test-key", nil)
	if err != nil {
		t.Fatalf("standard pool Run: %v", err)
	}
	defer func() { _ = result.Body.Close() }()

	if result.Member.Wire != "anthropic" {
		t.Errorf("standard pool should route to anthropic, got %s", result.Member.Wire)
	}

	body, _ := io.ReadAll(result.Body)
	if !strings.Contains(string(body), "message_start") {
		t.Error("expected message_start in response body")
	}
}

func TestCNProviderDegradationEnabled(t *testing.T) {
	// Full handler-stack test: degradation enabled → request routes through
	// anthropic_compat, features are stripped upstream, and degradation is recorded.
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("expected /v1/messages, got %s", r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	pol, _ := policy.NewEngine(&config.PolicyConfig{
		Version: "1.0",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "route", ModelPool: "standard"},
		},
		Degradation: config.DegradationConfig{
			AllowedCapabilities: []string{"cache_control", "extended_thinking"},
		},
	})

	pricing := &config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor: "deepseek-direct", Model: "deepseek-v4-pro",
				Capabilities: config.ModelCapabilities{CacheControl: false, ExtendedThinking: false},
			},
		},
	}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"deepseek-anthropic": {
				Wire:   "anthropic_compat",
				Vendor: "deepseek-direct",
				URL:    srv.URL,
				Supports: config.EndpointSupports{
					Streaming: true,
				},
			},
		},
		Pools: map[string]config.Pool{
			"standard": {
				Members:     []config.PoolMember{{EndpointID: "deepseek-anthropic", Model: "deepseek-v4-pro", Weight: 100}},
				MaxAttempts: 1,
			},
		},
	}, pricing)

	adapters := map[string]provider.Adapter{
		"anthropic_compat": provider.NewAnthropicCompatAdapter(),
	}

	cfg := &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"deepseek-anthropic": {ResolvedKey: "test-key"},
		},
		Policy: &config.PolicyConfig{
			Degradation: config.DegradationConfig{
				AllowedCapabilities: []string{"cache_control", "extended_thinking"},
			},
		},
	}

	egress := NewEgressPipeline(adapters, cost.NewCalculator(pricing), audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), cost.NewCalculator(pricing), cfg)
	handler := NewHandler(pipeline, nil, nil)

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Post("/v1/agent/forward", handler.HandleForward)

	reqBody := `{
		"envelope": {"trace_id": "t1"},
		"wire": {
			"protocol": "anthropic_messages",
			"stream": true,
			"body": {
				"model": "deepseek-v4-pro",
				"max_tokens": 100,
				"thinking": {"type": "enabled", "budget_tokens": 4000},
				"messages": [
					{"role": "user", "content": [{"type": "text", "text": "Hello", "cache_control": {"type": "ephemeral"}}]}
				]
			}
		}
	}`

	req := httptest.NewRequest("POST", "/v1/agent/forward", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for degradation-enabled request, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify upstream received stripped body (no cache_control, no thinking).
	if len(gotBody) == 0 {
		t.Fatal("upstream received no body")
	}
	var upstream struct {
		Thinking json.RawMessage `json:"thinking"`
		Messages []struct {
			Content []struct {
				CacheControl json.RawMessage `json:"cache_control,omitempty"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &upstream); err != nil {
		t.Fatalf("parse upstream body: %v (body=%s)", err, string(gotBody))
	}
	if len(upstream.Thinking) > 0 {
		t.Error("thinking should have been stripped from upstream body")
	}
	for _, msg := range upstream.Messages {
		for _, c := range msg.Content {
			if len(c.CacheControl) > 0 {
				t.Error("cache_control should have been stripped from upstream body")
			}
		}
	}

	// Verify aicg.usage event with degraded_features in the SSE response.
	bodyStr := rec.Body.String()
	if !strings.Contains(bodyStr, "aicg.usage") {
		t.Error("expected aicg.usage event in response")
	}
	if !strings.Contains(bodyStr, `"degraded_features":`) {
		t.Errorf("expected degraded_features in aicg.usage, got: %s", bodyStr)
	}
	if !strings.Contains(bodyStr, `"cache_control"`) {
		t.Error("expected cache_control in degraded_features")
	}
	if !strings.Contains(bodyStr, `"extended_thinking"`) {
		t.Error("expected extended_thinking in degraded_features")
	}

	// Verify routing headers.
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if rt := rec.Header().Get("X-AICG-Routed-To"); !strings.HasPrefix(rt, "deepseek-anthropic:") {
		t.Errorf("X-AICG-Routed-To = %q, want deepseek-anthropic:...", rt)
	}
}

func TestCNProviderDegradationStrictMode(t *testing.T) {
	// Full handler-stack test: degradation NOT configured → request with
	// cache_control + extended_thinking is rejected as no_eligible_endpoint.
	var upstreamHit atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit.Add(1)
		t.Error("upstream must not be called for strict-mode rejection")
		w.WriteHeader(500)
	}))
	defer srv.Close()

	pol, _ := policy.NewEngine(&config.PolicyConfig{
		Version: "1.0",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "route", ModelPool: "standard"},
		},
	})

	pricing := &config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor: "deepseek-direct", Model: "deepseek-v4-pro",
				Capabilities: config.ModelCapabilities{CacheControl: false, ExtendedThinking: false},
			},
		},
	}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"deepseek-anthropic": {
				Wire:   "anthropic_compat",
				Vendor: "deepseek-direct",
				URL:    srv.URL,
				Supports: config.EndpointSupports{
					Streaming: true,
				},
			},
		},
		Pools: map[string]config.Pool{
			"standard": {
				Members:     []config.PoolMember{{EndpointID: "deepseek-anthropic", Model: "deepseek-v4-pro", Weight: 100}},
				MaxAttempts: 1,
			},
		},
	}, pricing)

	adapters := map[string]provider.Adapter{
		"anthropic_compat": provider.NewAnthropicCompatAdapter(),
	}

	cfg := &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"deepseek-anthropic": {ResolvedKey: "test-key"},
		},
	}

	egress := NewEgressPipeline(adapters, cost.NewCalculator(pricing), audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), cost.NewCalculator(pricing), cfg)
	handler := NewHandler(pipeline, nil, nil)

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Post("/v1/agent/forward", handler.HandleForward)

	reqBody := `{
		"envelope": {"trace_id": "t1"},
		"wire": {
			"protocol": "anthropic_messages",
			"stream": true,
			"body": {
				"model": "deepseek-v4-pro",
				"max_tokens": 100,
				"thinking": {"type": "enabled", "budget_tokens": 4000},
				"messages": [
					{"role": "user", "content": [{"type": "text", "text": "Hello", "cache_control": {"type": "ephemeral"}}]}
				]
			}
		}
	}`

	req := httptest.NewRequest("POST", "/v1/agent/forward", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for strict-mode rejection, got %d: %s", rec.Code, rec.Body.String())
	}

	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("parse error response: %v", err)
	}
	if errResp.Code != "no_eligible_endpoint" {
		t.Errorf("error code = %q, want no_eligible_endpoint", errResp.Code)
	}
	if !strings.Contains(errResp.Message, "no candidate") {
		t.Errorf("error message should indicate no candidate, got: %q", errResp.Message)
	}

	if upstreamHit.Load() != 0 {
		t.Errorf("upstream received %d requests, want 0 (selector should reject before dispatch)", upstreamHit.Load())
	}
}

func TestHandlerForwardToolsReachUpstream(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	adapters := map[string]provider.Adapter{
		"anthropic_compat": provider.NewAnthropicCompatAdapter(),
	}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"deepseek-anthropic": {
				Wire:   "anthropic_compat",
				Vendor: "deepseek-direct",
				URL:    srv.URL,
				Supports: config.EndpointSupports{
					Streaming: true,
				},
			},
		},
		Pools: map[string]config.Pool{
			"standard": {
				Members:     []config.PoolMember{{EndpointID: "deepseek-anthropic", Model: "deepseek-v4-pro", Weight: 100}},
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

	calc := cost.NewCalculator(&config.PricingConfig{})
	egress := NewEgressPipeline(adapters, calc, audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), calc, &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"deepseek-anthropic": {ResolvedKey: "test-key"},
		},
	})
	handler := NewHandler(pipeline, nil, nil)

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Post("/v1/agent/forward", handler.HandleForward)

	reqBody := `{
		"envelope": {"trace_id": "t1"},
		"wire": {
			"protocol": "anthropic_messages",
			"stream": true,
			"body": {
				"model": "deepseek-v4-pro",
				"max_tokens": 100,
				"messages": [
					{"role": "user", "content": [{"type": "text", "text": "Write a file"}]}
				],
				"tools": [
					{
						"name": "write_file",
						"description": "Write a file to disk",
						"input_schema": {
							"type": "object",
							"properties": {
								"path": {"type": "string"},
								"content": {"type": "string"}
							},
							"required": ["path", "content"]
						}
					},
					{
						"name": "edit_file",
						"description": "Edit an existing file",
						"input_schema": {
							"type": "object",
							"properties": {
								"path": {"type": "string"},
								"old_string": {"type": "string"},
								"new_string": {"type": "string"}
							},
							"required": ["path", "old_string", "new_string"]
						}
					}
				],
				"tool_choice": {"type": "auto"}
			}
		}
	}`

	req := httptest.NewRequest("POST", "/v1/agent/forward", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if len(gotBody) == 0 {
		t.Fatal("upstream received no body")
	}

	var upstream struct {
		Tools      json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(gotBody, &upstream); err != nil {
		t.Fatalf("parse upstream body: %v (body=%s)", err, string(gotBody))
	}
	if len(upstream.Tools) == 0 {
		t.Error("tools missing from upstream body — tool definitions were not forwarded")
	}
	if len(upstream.ToolChoice) == 0 {
		t.Error("tool_choice missing from upstream body — tool_choice was not forwarded")
	}
}

func TestHandlerForwardRoutingModelOverride(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	adapters := map[string]provider.Adapter{
		"anthropic": provider.NewAnthropicAdapter(),
	}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"anthropic-prod": {
				Wire:   "anthropic",
				Vendor: "anthropic",
				URL:    srv.URL,
				Supports: config.EndpointSupports{
					Streaming: true,
				},
			},
		},
		Pools: map[string]config.Pool{
			"standard": {
				Members:     []config.PoolMember{{EndpointID: "anthropic-prod", Model: "claude-sonnet-4-6", Weight: 100}},
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

	calc := cost.NewCalculator(&config.PricingConfig{})
	egress := NewEgressPipeline(adapters, calc, audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), calc, &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"anthropic-prod": {ResolvedKey: "test-key"},
		},
	})
	handler := NewHandler(pipeline, nil, nil)

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Post("/v1/agent/forward", handler.HandleForward)

	reqBody := `{
		"envelope": {"trace_id": "t1"},
		"wire": {
			"protocol": "anthropic_messages",
			"stream": true,
			"body": {
				"model": "gpt-4o",
				"max_tokens": 100,
				"messages": [
					{"role": "user", "content": [{"type": "text", "text": "Hello"}]}
				]
			}
		}
	}`

	req := httptest.NewRequest("POST", "/v1/agent/forward", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if len(gotBody) == 0 {
		t.Fatal("upstream received no body")
	}

	var upstream struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(gotBody, &upstream); err != nil {
		t.Fatalf("parse upstream body: %v (body=%s)", err, string(gotBody))
	}
	if upstream.Model != "claude-sonnet-4-6" {
		t.Errorf("upstream model = %q, want claude-sonnet-4-6 (routing-selected model, not client's gpt-4o)", upstream.Model)
	}
}

func TestHandlerForwardStandardToAnthropicCompatToolsPreserved(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	adapters := map[string]provider.Adapter{
		"anthropic_compat": provider.NewAnthropicCompatAdapter(),
	}

	pricing := &config.PricingConfig{
		Models: []config.ModelPricing{
			{
				Vendor: "deepseek-direct", Model: "deepseek-v4-pro",
				Capabilities: config.ModelCapabilities{CacheControl: false, ExtendedThinking: false},
			},
		},
	}

	sel := routing.NewSelector(&config.PoolsConfig{
		ProviderEndpoints: map[string]config.ProviderEndpoint{
			"deepseek-anthropic": {
				Wire:   "anthropic_compat",
				Vendor: "deepseek-direct",
				URL:    srv.URL,
				Supports: config.EndpointSupports{
					Streaming: true,
				},
			},
		},
		Pools: map[string]config.Pool{
			"standard": {
				Members:     []config.PoolMember{{EndpointID: "deepseek-anthropic", Model: "deepseek-v4-pro", Weight: 100}},
				MaxAttempts: 1,
			},
		},
	}, pricing)

	pol, _ := policy.NewEngine(&config.PolicyConfig{
		Version: "1.0",
		Defaults: config.Defaults{
			OnNoMatch: config.ActionSpec{Action: "route", ModelPool: "standard"},
		},
		Degradation: config.DegradationConfig{
			AllowedCapabilities: []string{"cache_control", "extended_thinking"},
		},
	})

	egress := NewEgressPipeline(adapters, cost.NewCalculator(pricing), audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), cost.NewCalculator(pricing), &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"deepseek-anthropic": {ResolvedKey: "test-key"},
		},
		Policy: &config.PolicyConfig{
			Degradation: config.DegradationConfig{
				AllowedCapabilities: []string{"cache_control", "extended_thinking"},
			},
		},
	})
	handler := NewHandler(pipeline, nil, nil)

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Post("/v1/agent/forward", handler.HandleForward)

	reqBody := `{
		"envelope": {"trace_id": "t1"},
		"wire": {
			"protocol": "anthropic_messages",
			"stream": true,
			"body": {
				"model": "deepseek-v4-pro",
				"max_tokens": 100,
				"thinking": {"type": "enabled", "budget_tokens": 4000},
				"tools": [
					{
						"name": "write_file",
						"description": "Write a file to disk",
						"input_schema": {
							"type": "object",
							"properties": {
								"path": {"type": "string"},
								"content": {"type": "string"}
							},
							"required": ["path", "content"]
						}
					}
				],
				"tool_choice": {"type": "auto"},
				"messages": [
					{"role": "user", "content": [{"type": "text", "text": "Write a file", "cache_control": {"type": "ephemeral"}}]}
				]
			}
		}
	}`

	req := httptest.NewRequest("POST", "/v1/agent/forward", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if len(gotBody) == 0 {
		t.Fatal("upstream received no body")
	}

	var upstream struct {
		Tools      json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
		Thinking   json.RawMessage `json:"thinking"`
		Messages   []struct {
			Content []struct {
				CacheControl json.RawMessage `json:"cache_control,omitempty"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &upstream); err != nil {
		t.Fatalf("parse upstream body: %v (body=%s)", err, string(gotBody))
	}

	if len(upstream.Tools) == 0 {
		t.Error("tools missing from upstream body — tools must not be stripped by degradation")
	}
	if len(upstream.ToolChoice) == 0 {
		t.Error("tool_choice missing from upstream body — tool_choice must not be stripped by degradation")
	}
	if len(upstream.Thinking) > 0 {
		t.Error("thinking present in upstream body — must be stripped by degradation")
	}
	for _, msg := range upstream.Messages {
		for _, c := range msg.Content {
			if len(c.CacheControl) > 0 {
				t.Error("cache_control present in upstream body — must be stripped by degradation")
			}
		}
	}
}
