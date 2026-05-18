package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"agentgate/internal/gw/audit"
	"agentgate/internal/gw/auth"
	"agentgate/internal/gw/config"
	"agentgate/internal/gw/cost"
	"agentgate/internal/gw/edge"
	"agentgate/internal/gw/policy"
	"agentgate/internal/gw/provider"
	"agentgate/internal/gw/routing"
)

func TestIDORCrossUserAccess(t *testing.T) {
	store := auth.NewInMemKeyStore()

	// Seed two users in different teams.
	userAKey := "key-a-11111111-aaaaaaaa-bbbbcccc"
	userBKey := "key-b-22222222-ddddeeee-ffffgggg"
	_ = store.StoreAPIKey("user-a", "team-alpha", auth.RoleDeveloper, userAKey)
	_ = store.StoreAPIKey("user-b", "team-beta", auth.RoleDeveloper, userBKey)

	// Build a test router with auth middleware and a whoami endpoint.
	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Use(auth.AuthMiddleware(store))
	r.Get("/api/v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]string{
			"user_id": edge.GetUserID(r.Context()),
			"team_id": edge.GetTeamID(r.Context()),
			"role":    edge.GetRole(r.Context()),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	// Restricted endpoint: only platform_admin.
	r.With(auth.RequireRole(auth.RolePlatformAdmin)).Get("/api/v1/admin/users", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Test 1: User A presents own key → gets user A's identity.
	rec1 := doRequest(r, "GET", "/api/v1/whoami", userAKey)
	if rec1.Code != 200 {
		t.Fatalf("user A whoami: expected 200, got %d: %s", rec1.Code, rec1.Body.String())
	}
	if !strings.Contains(rec1.Body.String(), "user-a") {
		t.Errorf("user A key should return user-a identity, got: %s", rec1.Body.String())
	}
	if !strings.Contains(rec1.Body.String(), "team-alpha") {
		t.Errorf("user A key should return team-alpha, got: %s", rec1.Body.String())
	}

	// Test 2: User B presents own key → gets user B's identity.
	rec2 := doRequest(r, "GET", "/api/v1/whoami", userBKey)
	if rec2.Code != 200 {
		t.Fatalf("user B whoami: expected 200, got %d: %s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "user-b") {
		t.Errorf("user B key should return user-b identity, got: %s", rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), "team-beta") {
		t.Errorf("user B key should return team-beta, got: %s", rec2.Body.String())
	}

	// Test 3: User A cannot access admin endpoint (insufficient role) → 403.
	rec3 := doRequest(r, "GET", "/api/v1/admin/users", userAKey)
	if rec3.Code != 403 {
		t.Errorf("user A accessing admin: expected 403, got %d", rec3.Code)
	}

	// Test 4: User A's key does NOT return user B's identity (IDOR check).
	if strings.Contains(rec1.Body.String(), "user-b") {
		t.Error("IDOR: user A's key should NEVER return user B's identity")
	}
	if strings.Contains(rec1.Body.String(), "team-beta") {
		t.Error("IDOR: user A's key should NEVER return team-beta identity")
	}

	// Test 5: User B cannot access admin endpoint either → 403.
	rec5 := doRequest(r, "GET", "/api/v1/admin/users", userBKey)
	if rec5.Code != 403 {
		t.Errorf("user B accessing admin: expected 403, got %d", rec5.Code)
	}
}

func TestIDORCrossTenantAccess(t *testing.T) {
	store := auth.NewInMemKeyStore()

	userAKey := "key-a-33333333-1111111111111111"
	userBKey := "key-b-44444444-2222222222222222"
	_ = store.StoreAPIKey("alice", "tenant-alpha", auth.RoleTeamAdmin, userAKey)
	_ = store.StoreAPIKey("bob", "tenant-beta", auth.RoleDeveloper, userBKey)

	// TeamAdmin CAN access admin endpoints.
	// Developer CANNOT.

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Use(auth.AuthMiddleware(store))
	r.With(auth.RequireRole(auth.RoleTeamAdmin, auth.RolePlatformAdmin)).Get("/api/v1/team/members", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]string{
			"user_id": edge.GetUserID(r.Context()),
			"team_id": edge.GetTeamID(r.Context()),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	// Alice (team_admin) can access team endpoint.
	recA := doRequest(r, "GET", "/api/v1/team/members", userAKey)
	if recA.Code != 200 {
		t.Errorf("alice accessing team endpoint: expected 200, got %d: %s", recA.Code, recA.Body.String())
	}

	// Bob (developer) cannot access team endpoint → 403.
	recB := doRequest(r, "GET", "/api/v1/team/members", userBKey)
	if recB.Code != 403 {
		t.Errorf("bob accessing team endpoint: expected 403, got %d", recB.Code)
	}

	// IDOR: Alice's access returns alice's tenant, not bob's.
	if !strings.Contains(recA.Body.String(), "alice") {
		t.Errorf("alice key should return alice identity, got: %s", recA.Body.String())
	}
	if strings.Contains(recA.Body.String(), "bob") || strings.Contains(recA.Body.String(), "tenant-beta") {
		t.Error("IDOR: alice's key should NEVER return bob's identity or tenant")
	}
}

func TestSetupTokenExchangeMintsKey(t *testing.T) {
	store := auth.NewInMemKeyStore()
	st := &auth.SetupToken{Token: "valid-setup-token-12345678"}

	if store.HasAnyKey() {
		t.Fatal("store should be empty before exchange")
	}

	pipeline := NewPipeline(nil, nil, nil, nil, nil, nil)
	handler := NewHandler(pipeline, store, st)

	// Exchange endpoint is mounted WITHOUT auth middleware so the auth layer
	// doesn't accidentally consume the setup_token before the handler can.
	r := chi.NewRouter()
	r.Post("/api/v1/lp/exchange-setup", handler.HandleExchangeSetup)

	// Exchange setup token for a real API key.
	req := httptest.NewRequest("POST", "/api/v1/lp/exchange-setup", nil)
	req.Header.Set("Authorization", "Bearer valid-setup-token-12345678")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		APIKey string `json:"api_key"`
		UserID string `json:"user_id"`
		Role   string `json:"role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.APIKey == "" {
		t.Fatal("expected non-empty api_key in response")
	}
	if resp.Role != auth.RolePlatformAdmin {
		t.Errorf("expected platform_admin role, got %q", resp.Role)
	}
	if !store.HasAnyKey() {
		t.Fatal("store should have a key after exchange")
	}
	if !st.Consumed {
		t.Error("setup token should be consumed")
	}

	// Attempting to exchange again with the same (consumed) token must fail.
	req2 := httptest.NewRequest("POST", "/api/v1/lp/exchange-setup", nil)
	req2.Header.Set("Authorization", "Bearer valid-setup-token-12345678")
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != 410 {
		t.Errorf("second exchange: expected 410 Gone, got %d", rec2.Code)
	}
}

func TestSetupTokenExchangeInvalidToken(t *testing.T) {
	store := auth.NewInMemKeyStore()
	st := &auth.SetupToken{Token: "right-token-1234567890ab"}

	pipeline := NewPipeline(nil, nil, nil, nil, nil, nil)
	handler := NewHandler(pipeline, store, st)

	// Exchange endpoint mounted WITHOUT auth middleware.
	r := chi.NewRouter()
	r.Post("/api/v1/lp/exchange-setup", handler.HandleExchangeSetup)

	req := httptest.NewRequest("POST", "/api/v1/lp/exchange-setup", nil)
	req.Header.Set("Authorization", "Bearer wrong-token-xxxxxxxxxx")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 403 {
		t.Errorf("expected 403 for wrong token, got %d: %s", rec.Code, rec.Body.String())
	}
	if st.Consumed {
		t.Error("token should NOT be consumed on failed exchange")
	}
}

// Helpers

func traceIDStub(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctx = contextWith(ctx, edge.CtxTraceID, "test-trace-id")
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func contextWith(parent context.Context, key, val any) context.Context {
	return &ctxNode{Context: parent, key: key, val: val}
}

type ctxNode struct {
	context.Context
	key any
	val any
}

func (c *ctxNode) Value(key any) any {
	if key == c.key {
		return c.val
	}
	return c.Context.Value(key)
}

func doRequest(r *chi.Mux, method, path, apiKey string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// errorAdapter is a mock provider.Adapter that returns a fixed error from Stream.
type errorAdapter struct {
	name string
	err  error
}

func (a *errorAdapter) Name() string                                          { return a.name }
func (a *errorAdapter) CountTokens(_ context.Context, _ *provider.ProviderRequest) (int, error) {
	return 0, nil
}
func (a *errorAdapter) NonStream(_ context.Context, _ *provider.ProviderRequest) ([]byte, error) {
	return nil, a.err
}
func (a *errorAdapter) Stream(_ context.Context, _ *provider.ProviderRequest) (io.ReadCloser, error) {
	return nil, a.err
}

func TestHandleForwardUpstreamError4xx(t *testing.T) {
	tests := []struct {
		name         string
		upstreamCode int
		wantCode     int
		wantErrCode  string
	}{
		{"404", http.StatusNotFound, http.StatusNotFound, "provider_404"},
		{"401", http.StatusUnauthorized, http.StatusUnauthorized, "provider_401"},
		{"429", http.StatusTooManyRequests, http.StatusTooManyRequests, "provider_429"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			adapter := &errorAdapter{
				name: "anthropic",
				err: &provider.UpstreamError{
					Status:   tt.upstreamCode,
					Body:     `{"error":"upstream error"}`,
					Provider: "anthropic",
				},
			}

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

			cfg := &config.Config{
				ResolvedEndpoints: map[string]config.EndpointWithKey{
					"anthropic-prod": {ResolvedKey: "test-key"},
				},
			}

			egress := NewEgressPipeline(map[string]provider.Adapter{"anthropic": adapter}, cost.NewCalculator(nil), audit.NewWriter(), sel)
			pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), cost.NewCalculator(nil), cfg)
			handler := NewHandler(pipeline, nil, nil)

			r := chi.NewRouter()
			r.Use(traceIDStub)
			r.Post("/v1/agent/forward", handler.HandleForward)

			body := `{"envelope":{"trace_id":"t1"},"wire":{"protocol":"anthropic_messages","stream":true,"body":{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]}]}}}`
			req := httptest.NewRequest("POST", "/v1/agent/forward", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}

			var errResp ErrorResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
				t.Fatalf("parse error response: %v", err)
			}
			if errResp.Code != tt.wantErrCode {
				t.Errorf("code = %q, want %q", errResp.Code, tt.wantErrCode)
			}
			if errResp.TraceID != "test-trace-id" {
				t.Errorf("trace_id = %q, want test-trace-id", errResp.TraceID)
			}
			if !strings.Contains(errResp.Message, "upstream error") {
				t.Errorf("message = %q, want it to contain upstream body", errResp.Message)
			}
		})
	}
}

// countAdapter records whether Stream was called.
type countAdapter struct {
	name    string
	streams int
}

func (a *countAdapter) Name() string                                               { return a.name }
func (a *countAdapter) CountTokens(_ context.Context, _ *provider.ProviderRequest) (int, error) { return 0, nil }
func (a *countAdapter) NonStream(_ context.Context, _ *provider.ProviderRequest) ([]byte, error) { return nil, nil }
func (a *countAdapter) Stream(_ context.Context, _ *provider.ProviderRequest) (io.ReadCloser, error) {
	a.streams++
	return io.NopCloser(strings.NewReader("data: ok\n\n")), nil
}

func TestHandleForwardBadRequestErrorMalformedBody(t *testing.T) {
	adapter := &countAdapter{name: "anthropic"}

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

	cfg := &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"anthropic-prod": {ResolvedKey: "test-key"},
		},
	}

	egress := NewEgressPipeline(map[string]provider.Adapter{"anthropic": adapter}, cost.NewCalculator(nil), audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), cost.NewCalculator(nil), cfg)
	handler := NewHandler(pipeline, nil, nil)

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Post("/v1/agent/forward", handler.HandleForward)

	// Malformed Anthropic wire body: content is a number, not string or array.
	body := `{"envelope":{"trace_id":"t1"},"wire":{"protocol":"anthropic_messages","stream":true,"body":{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":123}]}}}`
	req := httptest.NewRequest("POST", "/v1/agent/forward", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d. body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("parse error response: %v", err)
	}
	if errResp.Code != "bad_request" {
		t.Errorf("code = %q, want bad_request", errResp.Code)
	}
	if errResp.TraceID != "test-trace-id" {
		t.Errorf("trace_id = %q, want test-trace-id", errResp.TraceID)
	}
	if !strings.Contains(errResp.Message, "parse anthropic request") {
		t.Errorf("message = %q, want it to contain 'parse anthropic request'", errResp.Message)
	}
	if !strings.Contains(errResp.Message, "cannot unmarshal") {
		t.Errorf("message = %q, want it to contain parse cause substring 'cannot unmarshal'", errResp.Message)
	}

	// The upstream/provider adapter must NOT have been called.
	if adapter.streams != 0 {
		t.Errorf("adapter.Stream was called %d times, want 0 — malformed input must not reach the provider", adapter.streams)
	}
}

func TestHandleForwardBadRequestPreservesProvider4xxPassthrough(t *testing.T) {
	adapter := &errorAdapter{
		name: "anthropic",
		err: &provider.UpstreamError{
			Status:   http.StatusTooManyRequests,
			Body:     `{"error":"rate limited"}`,
			Provider: "anthropic",
		},
	}

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

	cfg := &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"anthropic-prod": {ResolvedKey: "test-key"},
		},
	}

	egress := NewEgressPipeline(map[string]provider.Adapter{"anthropic": adapter}, cost.NewCalculator(nil), audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), cost.NewCalculator(nil), cfg)
	handler := NewHandler(pipeline, nil, nil)

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Post("/v1/agent/forward", handler.HandleForward)

	body := `{"envelope":{"trace_id":"t1"},"wire":{"protocol":"anthropic_messages","stream":true,"body":{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}}}`
	req := httptest.NewRequest("POST", "/v1/agent/forward", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusTooManyRequests)
	}

	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("parse error response: %v", err)
	}
	// Provider 4xx must NOT be converted to bad_request.
	if errResp.Code != "provider_429" {
		t.Errorf("code = %q, want provider_429 — provider 4xx must remain distinct from client bad_request", errResp.Code)
	}
}

func TestHandleForwardNestedToolResultStringContent(t *testing.T) {
	adapter := &countAdapter{name: "anthropic"}

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

	cfg := &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"anthropic-prod": {ResolvedKey: "test-key"},
		},
	}

	egress := NewEgressPipeline(map[string]provider.Adapter{"anthropic": adapter}, cost.NewCalculator(nil), audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), cost.NewCalculator(nil), cfg)
	handler := NewHandler(pipeline, nil, nil)

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Post("/v1/agent/forward", handler.HandleForward)

	// Valid request with nested tool_result.content as a string.
	body := `{"envelope":{"trace_id":"t1"},"wire":{"protocol":"anthropic_messages","stream":true,"body":{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_001","content":"file contents here"}]}]}}}`
	req := httptest.NewRequest("POST", "/v1/agent/forward", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d. body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	// The provider adapter must have been called (nested string shorthand parsed successfully).
	if adapter.streams != 1 {
		t.Errorf("adapter.Stream was called %d times, want 1 — valid nested string content must reach provider", adapter.streams)
	}
}

func TestHandleForwardNestedMalformedContent(t *testing.T) {
	adapter := &countAdapter{name: "anthropic"}

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

	cfg := &config.Config{
		ResolvedEndpoints: map[string]config.EndpointWithKey{
			"anthropic-prod": {ResolvedKey: "test-key"},
		},
	}

	egress := NewEgressPipeline(map[string]provider.Adapter{"anthropic": adapter}, cost.NewCalculator(nil), audit.NewWriter(), sel)
	pipeline := NewPipeline(pol, sel, egress, audit.NewWriter(), cost.NewCalculator(nil), cfg)
	handler := NewHandler(pipeline, nil, nil)

	r := chi.NewRouter()
	r.Use(traceIDStub)
	r.Post("/v1/agent/forward", handler.HandleForward)

	// Malformed request: nested tool_result.content is a number, not string or array.
	body := `{"envelope":{"trace_id":"t1"},"wire":{"protocol":"anthropic_messages","stream":true,"body":{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool_001","content":123}]}]}}}`
	req := httptest.NewRequest("POST", "/v1/agent/forward", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d. body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	var errResp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("parse error response: %v", err)
	}
	if errResp.Code != "bad_request" {
		t.Errorf("code = %q, want bad_request", errResp.Code)
	}
	if errResp.TraceID != "test-trace-id" {
		t.Errorf("trace_id = %q, want test-trace-id", errResp.TraceID)
	}
	if !strings.Contains(errResp.Message, "parse anthropic request") {
		t.Errorf("message = %q, want it to contain 'parse anthropic request'", errResp.Message)
	}
	if !strings.Contains(errResp.Message, "cannot unmarshal") {
		t.Errorf("message = %q, want it to contain parse cause substring 'cannot unmarshal'", errResp.Message)
	}

	// The upstream/provider adapter must NOT have been called.
	if adapter.streams != 0 {
		t.Errorf("adapter.Stream was called %d times, want 0 — malformed nested content must not reach the provider", adapter.streams)
	}
}
