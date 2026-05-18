package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentgate/internal/lp/gwclient"
	"agentgate/internal/lp/session"
)

func TestAnthropicHandlerPassesThroughGW4xx(t *testing.T) {
	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-AICG-Trace-Id", "trace-4xx-001")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":"bad_request","message":"parse anthropic request: cannot unmarshal number","trace_id":"trace-4xx-001"}`))
	}))
	defer gwSrv.Close()

	client := gwclient.NewClient(gwSrv.URL, "test-key", "")
	handler := &AnthropicHandler{
		GwClient: client,
		Session:  session.New("/tmp"),
		RepoRoot: "/tmp",
	}

	mux := http.NewServeMux()
	handler.Register(mux)

	// Test messages handler
	t.Run("messages_4xx_passthrough", func(t *testing.T) {
		body := `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":123}],"stream":true}`
		req := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("messages handler: expected status 400, got %d. body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "bad_request") {
			t.Errorf("messages handler: expected body to contain 'bad_request', got: %s", rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "cannot unmarshal") {
			t.Errorf("messages handler: expected body to contain parse cause 'cannot unmarshal', got: %s", rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("messages handler: expected Content-Type application/json, got: %s", ct)
		}
	})

	// Test count_tokens handler
	t.Run("count_tokens_4xx_passthrough", func(t *testing.T) {
		body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":123}]}`
		req := httptest.NewRequest("POST", "/anthropic/v1/messages/count_tokens", strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("count_tokens handler: expected status 400, got %d. body: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "bad_request") {
			t.Errorf("count_tokens handler: expected body to contain 'bad_request', got: %s", rec.Body.String())
		}
	})
}

func TestAnthropicHandlerGeneric502OnGWError(t *testing.T) {
	// GW returns 500 twice (so LP retry exhausts and returns an error).
	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"internal_error","message":"something broke","trace_id":"trace-5xx-001"}`))
	}))
	defer gwSrv.Close()

	client := gwclient.NewClient(gwSrv.URL, "test-key", "")
	handler := &AnthropicHandler{
		GwClient: client,
		Session:  session.New("/tmp"),
		RepoRoot: "/tmp",
	}

	mux := http.NewServeMux()
	handler.Register(mux)

	t.Run("messages_generic_502", func(t *testing.T) {
		body := `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"stream":true}`
		req := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("messages handler: expected status 502, got %d. body: %s", rec.Code, rec.Body.String())
		}
		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp["error"] != "gateway unreachable" {
			t.Errorf("expected 'gateway unreachable', got: %s", resp["error"])
		}
		// Generic 502 must not expose raw GW error codes.
		if strings.Contains(rec.Body.String(), "internal_error") {
			t.Error("generic 502 must not leak GW error code to the client")
		}
	})

	t.Run("count_tokens_generic_502", func(t *testing.T) {
		body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest("POST", "/anthropic/v1/messages/count_tokens", strings.NewReader(body))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadGateway {
			t.Fatalf("count_tokens handler: expected status 502, got %d. body: %s", rec.Code, rec.Body.String())
		}
		var resp map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("parse response: %v", err)
		}
		if resp["error"] != "gateway unreachable" {
			t.Errorf("expected 'gateway unreachable', got: %s", resp["error"])
		}
	})
}

func TestSSERelayFiltersUsageAndPreservesNormalFrames(t *testing.T) {
	gwBody := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\nevent: aicg.usage\ndata: {\"trace_id\":\"trace-relay-001\",\"cost_cents\":1,\"tokens\":{\"input\":10,\"output\":5},\"routed_to\":\"anthropic:claude-sonnet-4-6\"}\n\n"

	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-AICG-Trace-Id", "trace-relay-001")
		w.Header().Set("X-AICG-Routed-To", "anthropic:claude-sonnet-4-6")
		w.Header().Set("Content-Length", "999")
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("Trailer", "X-Custom")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(gwBody))
	}))
	defer gwSrv.Close()

	client := gwclient.NewClient(gwSrv.URL, "test-key", "")
	handler := &AnthropicHandler{
		GwClient: client,
		Session:  session.New("/tmp"),
		RepoRoot: "/tmp",
	}

	mux := http.NewServeMux()
	handler.Register(mux)

	body := `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"stream":true}`
	req := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	resp := rec.Body.String()

	// Must contain expected Anthropic frames.
	for _, want := range []string{"event: message_start", "content_block_delta", "message_delta", "message_stop"} {
		if !strings.Contains(resp, want) {
			t.Errorf("response missing expected Anthropic frame %q", want)
		}
	}

	// Must NOT contain aicg.* frames.
	if strings.Contains(resp, "event: aicg.") {
		t.Errorf("client response must not contain aicg.* frames, got: %s", resp)
	}

	// Unsafe headers must be stripped.
	for _, h := range []string{"Content-Length", "Transfer-Encoding", "Connection", "Trailer"} {
		if rec.Header().Get(h) != "" {
			t.Errorf("header %q should be stripped, got: %q", h, rec.Header().Get(h))
		}
	}

	// Safe headers must be preserved.
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type should be text/event-stream, got: %q", ct)
	}
	if trace := rec.Header().Get("X-AICG-Trace-Id"); trace != "trace-relay-001" {
		t.Errorf("X-AICG-Trace-Id should be preserved, got: %q", trace)
	}
	if routed := rec.Header().Get("X-AICG-Routed-To"); routed != "anthropic:claude-sonnet-4-6" {
		t.Errorf("X-AICG-Routed-To should be preserved, got: %q", routed)
	}
}

func TestSSERelayTranslatesAicgError(t *testing.T) {
	gwBody := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hel\"}}\n\nevent: aicg.error\ndata: {\"message\":\"provider timeout after first byte\",\"trace_id\":\"trace-err-001\"}\n\nevent: aicg.usage\ndata: {\"trace_id\":\"trace-err-001\",\"cost_cents\":0,\"tokens\":{\"input\":10,\"output\":3},\"routed_to\":\"anthropic:claude-sonnet-4-6\"}\n\n"

	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(gwBody))
	}))
	defer gwSrv.Close()

	client := gwclient.NewClient(gwSrv.URL, "test-key", "")
	handler := &AnthropicHandler{
		GwClient: client,
		Session:  session.New("/tmp"),
		RepoRoot: "/tmp",
	}

	mux := http.NewServeMux()
	handler.Register(mux)

	body := `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"stream":true}`
	req := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	resp := rec.Body.String()

	// Must NOT contain raw aicg.* frames.
	if strings.Contains(resp, "event: aicg.") {
		t.Errorf("client response must not contain aicg.* frames, got: %s", resp)
	}

	// Must contain Anthropic error translation.
	if !strings.Contains(resp, "event: error") {
		t.Errorf("response must contain event: error, got: %s", resp)
	}
	if !strings.Contains(resp, `"type":"error"`) {
		t.Errorf("error frame must have type:error, got: %s", resp)
	}
	if !strings.Contains(resp, `"type":"api_error"`) {
		t.Errorf("error frame must have error.type:api_error, got: %s", resp)
	}
	if !strings.Contains(resp, "provider timeout after first byte") {
		t.Errorf("error message must include original aicg message, got: %s", resp)
	}
	if !strings.Contains(resp, "trace-err-001") {
		t.Errorf("error message must include trace_id, got: %s", resp)
	}

	// Must NOT include aicg.usage after error (stream closes at error).
	if strings.Contains(resp, "aicg.usage") {
		t.Errorf("aicg.usage must not appear after error frame, got: %s", resp)
	}

	// Normal frames before the error must be forwarded.
	if !strings.Contains(resp, "message_start") {
		t.Errorf("normal frames before error must be forwarded, got: %s", resp)
	}
	if !strings.Contains(resp, "content_block_delta") {
		t.Errorf("normal frames before error must be forwarded, got: %s", resp)
	}
}

func TestSSERelayHandlesSplitFrames(t *testing.T) {
	// SSE frames split across arbitrary write boundaries.
	parts := []string{
		"event: message_start\ndata: {\"type\":\"mes",
		"sage_start\"}\n\nevent: content_block_",
		"delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello\"}}\n\nevent: aicg.us",
		"age\ndata: {\"trace_id\":\"trace-split-001\",\"cost_cents\":1,\"tokens\":{\"input\":10,\"output\":5},\"routed_to\":\"anthropic:claude-sonnet-4-6\"}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, p := range parts {
			_, _ = w.Write([]byte(p))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer gwSrv.Close()

	client := gwclient.NewClient(gwSrv.URL, "test-key", "")
	handler := &AnthropicHandler{
		GwClient: client,
		Session:  session.New("/tmp"),
		RepoRoot: "/tmp",
	}

	mux := http.NewServeMux()
	handler.Register(mux)

	body := `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"stream":true}`
	req := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	resp := rec.Body.String()

	if !strings.Contains(resp, "message_start") {
		t.Errorf("split frame: missing message_start, got: %s", resp)
	}
	if !strings.Contains(resp, "message_stop") {
		t.Errorf("split frame: missing message_stop, got: %s", resp)
	}
	if strings.Contains(resp, "aicg.") {
		t.Errorf("split frame: aicg.* must not appear, got: %s", resp)
	}
}

func TestSSERelayHandlesLargeFrame(t *testing.T) {
	// Build a frame with a data line exceeding 64 KiB.
	largeData := `{"type":"content_block_delta","delta":{"text":"` + strings.Repeat("x", 70000) + `"}}`
	gwBody := "event: content_block_delta\ndata: " + largeData + "\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(gwBody))
	}))
	defer gwSrv.Close()

	client := gwclient.NewClient(gwSrv.URL, "test-key", "")
	handler := &AnthropicHandler{
		GwClient: client,
		Session:  session.New("/tmp"),
		RepoRoot: "/tmp",
	}

	mux := http.NewServeMux()
	handler.Register(mux)

	body := `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"stream":true}`
	req := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	resp := rec.Body.String()

	if !strings.Contains(resp, "content_block_delta") {
		t.Errorf("large frame: missing content_block_delta, got: %s", resp)
	}
	if !strings.Contains(resp, strings.Repeat("x", 70000)) {
		t.Errorf("large frame: 70KB payload not preserved intact")
	}
	if !strings.Contains(resp, "message_stop") {
		t.Errorf("large frame: missing message_stop, got: %s", resp)
	}
}

func TestSSERelayNonStreamingUnchanged(t *testing.T) {
	// Non-streaming 200 responses must not go through the relay.
	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "50")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message","content":[{"text":"hi"}]}`))
	}))
	defer gwSrv.Close()

	client := gwclient.NewClient(gwSrv.URL, "test-key", "")
	handler := &AnthropicHandler{
		GwClient: client,
		Session:  session.New("/tmp"),
		RepoRoot: "/tmp",
	}

	mux := http.NewServeMux()
	handler.Register(mux)

	body := `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"stream":false}`
	req := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	// Non-streaming preserves Content-Length (header filtering not applied).
	if rec.Header().Get("Content-Length") == "" {
		t.Error("non-streaming should preserve Content-Length header")
	}
	// Body passes through unchanged.
	if !strings.Contains(rec.Body.String(), `"text":"hi"`) {
		t.Errorf("non-streaming body should pass through, got: %s", rec.Body.String())
	}
}

// TestSSERelayRegressionClaudeCodeJSONParseError documents the regression
// scenario that this handoff fixes: GW returns a normal Anthropic stream ending
// with message_stop followed by aicg.usage, and LP previously forwarded the
// aicg.usage frame to Claude Code, which caused "JSON Parse error: Unexpected
// identifier" because the client tried to parse the non-Anthropic SSE payload.
// After the fix, the LP-facing stream must contain only Anthropic frames.
func TestSSERelayRegressionClaudeCodeJSONParseError(t *testing.T) {
	// Simulate the exact GW response shape: normal Anthropic frames ending with
	// message_stop, followed by aicg.usage. This reproduces the stream that
	// caused Claude Code to fail with a JSON parse error.
	gwBody := "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\"}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"I\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\"}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\nevent: aicg.usage\ndata: {\"trace_id\":\"trace-regression-001\",\"cost_cents\":1,\"tokens\":{\"input\":10,\"output\":1},\"routed_to\":\"anthropic:claude-sonnet-4-6\"}\n\n"

	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-AICG-Trace-Id", "trace-regression-001")
		w.Header().Set("X-AICG-Routed-To", "anthropic:claude-sonnet-4-6")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(gwBody))
	}))
	defer gwSrv.Close()

	client := gwclient.NewClient(gwSrv.URL, "test-key", "")
	handler := &AnthropicHandler{
		GwClient: client,
		Session:  session.New("/tmp"),
		RepoRoot: "/tmp",
	}

	mux := http.NewServeMux()
	handler.Register(mux)

	body := `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"stream":true}`
	req := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	resp := rec.Body.String()

	// The LP-facing stream must contain all Anthropic frames.
	for _, want := range []string{"event: message_start", "content_block_delta", "message_delta", "message_stop"} {
		if !strings.Contains(resp, want) {
			t.Errorf("response missing expected Anthropic frame %q", want)
		}
	}

	// The LP-facing stream must NOT contain any aicg.* frames — this is the
	// regression fix: Claude Code must never see AgentGate internal metadata.
	if strings.Contains(resp, "event: aicg.") {
		t.Errorf("regression: client response contains aicg.* frames, which would cause Claude Code JSON parse errors. Response: %s", resp)
	}

	// message_stop must be the last Anthropic frame the client sees.
	msgStopIdx := strings.LastIndex(resp, "message_stop")
	aicgIdx := strings.LastIndex(resp, "aicg.")
	if aicgIdx > msgStopIdx {
		t.Errorf("aicg.* frame appears after message_stop in client response")
	}
}

func TestSSERelayPreservesFlushSemantics(t *testing.T) {
	gwBody := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	gwSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(gwBody))
	}))
	defer gwSrv.Close()

	client := gwclient.NewClient(gwSrv.URL, "test-key", "")
	handler := &AnthropicHandler{
		GwClient: client,
		Session:  session.New("/tmp"),
		RepoRoot: "/tmp",
	}

	mux := http.NewServeMux()
	handler.Register(mux)

	body := `{"model":"claude-sonnet-4-6","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"stream":true}`
	req := httptest.NewRequest("POST", "/anthropic/v1/messages", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	resp := rec.Body.String()
	if !strings.Contains(resp, "\n\n") {
		t.Error("response should contain SSE frame delimiters")
	}
}