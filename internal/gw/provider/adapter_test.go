package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAnthropicAdapterStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			t.Error("expected x-api-key header")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	a := NewAnthropicAdapter()
	rc, err := a.Stream(context.Background(), &ProviderRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []byte(`[]`),
		Stream:   true,
		APIKey:   "test-key",
		URL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = rc.Close() }()
	data, _ := io.ReadAll(rc)
	if !strings.Contains(string(data), "message_start") {
		t.Errorf("expected message_start in response, got: %s", string(data))
	}
}

func TestAnthropicAdapterNonStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"Hello"}]}`))
	}))
	defer srv.Close()

	a := NewAnthropicAdapter()
	data, err := a.NonStream(context.Background(), &ProviderRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []byte(`[]`),
		APIKey:   "test-key",
		URL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("NonStream: %v", err)
	}
	if !strings.Contains(string(data), "Hello") {
		t.Errorf("expected Hello in response, got: %s", string(data))
	}
}

func TestAnthropicAdapterCountTokens(t *testing.T) {
	a := NewAnthropicAdapter()
	n, err := a.CountTokens(context.Background(), &ProviderRequest{Model: "claude-sonnet-4-6"})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 0 {
		t.Errorf("P0 count_tokens should return 0 (local tokenizer not integrated), got %d", n)
	}
}

func TestAnthropicAdapterName(t *testing.T) {
	a := NewAnthropicAdapter()
	if a.Name() != "anthropic" {
		t.Errorf("Name() = %q", a.Name())
	}
}

func TestAnthropicAdapterHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()

	a := NewAnthropicAdapter()
	_, err := a.Stream(context.Background(), &ProviderRequest{
		Model:    "x",
		Messages: []byte(`[]`),
		Stream:   true,
		URL:      srv.URL,
	})
	if err == nil {
		t.Fatal("expected error for 503")
	}
}

func TestOpenAICompatAdapterStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	a := NewOpenAICompatAdapter()
	rc, err := a.Stream(context.Background(), &ProviderRequest{
		Model:    "gpt-4o",
		Messages: []byte(`{"model":"gpt-4o","messages":[]}`),
		Stream:   true,
		APIKey:   "test-key",
		URL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer func() { _ = rc.Close() }()
	data, _ := io.ReadAll(rc)
	if !strings.Contains(string(data), "Hi") {
		t.Errorf("expected Hi in response, got: %s", string(data))
	}
}

func TestOpenAICompatAdapterNonStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Hello"}}]}`))
	}))
	defer srv.Close()

	a := NewOpenAICompatAdapter()
	data, err := a.NonStream(context.Background(), &ProviderRequest{
		Model:    "gpt-4o",
		Messages: []byte(`{"model":"gpt-4o","messages":[]}`),
		APIKey:   "test-key",
		URL:      srv.URL,
	})
	if err != nil {
		t.Fatalf("NonStream: %v", err)
	}
	if !strings.Contains(string(data), "Hello") {
		t.Errorf("expected Hello, got: %s", string(data))
	}
}

func TestOpenAICompatAdapterCountTokens(t *testing.T) {
	a := NewOpenAICompatAdapter()
	n, _ := a.CountTokens(context.Background(), &ProviderRequest{Model: "gpt-4o"})
	if n != 0 {
		t.Errorf("P0 count_tokens should return 0, got %d", n)
	}
}

func TestOpenAICompatAdapterName(t *testing.T) {
	a := NewOpenAICompatAdapter()
	if a.Name() != "openai_compat" {
		t.Errorf("Name() = %q", a.Name())
	}
}

func TestAnthropicAdapterUpstreamError4xx(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{"404", http.StatusNotFound},
		{"401", http.StatusUnauthorized},
		{"429", http.StatusTooManyRequests},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(`{"error":"something went wrong"}`))
			}))
			defer srv.Close()

			a := NewAnthropicAdapter()
			_, err := a.Stream(context.Background(), &ProviderRequest{
				Model:    "claude-sonnet-4-6",
				Messages: []byte(`[]`),
				Stream:   true,
				APIKey:   "test-key",
				URL:      srv.URL,
			})

			var upErr *UpstreamError
			if !errors.As(err, &upErr) {
				t.Fatalf("expected *UpstreamError, got %T: %v", err, err)
			}
			if upErr.Status != tt.statusCode {
				t.Errorf("status = %d, want %d", upErr.Status, tt.statusCode)
			}
			if upErr.Provider != "anthropic" {
				t.Errorf("provider = %q, want anthropic", upErr.Provider)
			}
			if !strings.Contains(upErr.Body, "something went wrong") {
				t.Errorf("body = %q, want it to contain upstream response", upErr.Body)
			}
		})
	}
}

func TestAnthropicAdapter5xxNotUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	a := NewAnthropicAdapter()
	_, err := a.Stream(context.Background(), &ProviderRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []byte(`[]`),
		Stream:   true,
		APIKey:   "test-key",
		URL:      srv.URL,
	})

	if err == nil {
		t.Fatal("expected error for 502")
	}
	var upErr *UpstreamError
	if errors.As(err, &upErr) {
		t.Fatalf("5xx should not be UpstreamError, got %v", upErr)
	}
}

func TestOpenAICompatAdapterHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()

	a := NewOpenAICompatAdapter()
	_, err := a.Stream(context.Background(), &ProviderRequest{
		Model:    "x",
		Messages: []byte(`{"model":"x","messages":[]}`),
		Stream:   true,
		URL:      srv.URL,
	})
	if err == nil {
		t.Fatal("expected error for 500")
	}
}

func TestAnthropicAdapterRequestBodyContainsAllTopLevelFields(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: message_delta\ndata: {}\n\n"))
	}))
	defer srv.Close()

	a := NewAnthropicAdapter()
	rc, err := a.Stream(context.Background(), &ProviderRequest{
		Model:      "claude-sonnet-4-6",
		Messages:   []byte(`[{"role":"user","content":[{"type":"text","text":"Write a file"}]}]`),
		MaxTokens:  1024,
		Stream:     true,
		APIKey:     "test-key",
		URL:        srv.URL,
		System:     []byte(`[{"type":"text","text":"You are a helpful assistant"}]`),
		Tools:      []byte(`[{"name":"write_file","description":"Write a file to disk","input_schema":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}}]`),
		ToolChoice: []byte(`{"type":"auto"}`),
		Metadata:   map[string]string{"user_id": "user-123"},
		Thinking:   []byte(`{"type":"enabled","budget_tokens":16000}`),
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = rc.Close()

	if len(gotBody) == 0 {
		t.Fatal("upstream received no body")
	}
	var upstream map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &upstream); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}

	if _, ok := upstream["system"]; !ok {
		t.Error("system missing from upstream body")
	}
	if _, ok := upstream["tools"]; !ok {
		t.Error("tools missing from upstream body")
	}
	if _, ok := upstream["tool_choice"]; !ok {
		t.Error("tool_choice missing from upstream body")
	}
	if _, ok := upstream["metadata"]; !ok {
		t.Error("metadata missing from upstream body")
	}
	if _, ok := upstream["thinking"]; !ok {
		t.Error("thinking missing from upstream body")
	}
}

func TestAnthropicAdapterRequestBodyOmitsEmptyOptionalFields(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: message_delta\ndata: {}\n\n"))
	}))
	defer srv.Close()

	a := NewAnthropicAdapter()
	rc, err := a.Stream(context.Background(), &ProviderRequest{
		Model:    "claude-sonnet-4-6",
		Messages: []byte(`[{"role":"user","content":[{"type":"text","text":"Hello"}]}]`),
		MaxTokens: 100,
		Stream:    true,
		APIKey:    "test-key",
		URL:       srv.URL,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = rc.Close()

	if len(gotBody) == 0 {
		t.Fatal("upstream received no body")
	}
	var upstream map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &upstream); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}

	for _, field := range []string{"system", "tools", "tool_choice", "metadata", "thinking"} {
		if _, ok := upstream[field]; ok {
			t.Errorf("%s should be omitted when absent, but was present in upstream body", field)
		}
	}
}

func TestAnthropicAdapterRequestBodyRoutingModelUsed(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: message_delta\ndata: {}\n\n"))
	}))
	defer srv.Close()

	// Model from routing — may differ from the original request model.
	a := NewAnthropicAdapter()
	rc, err := a.Stream(context.Background(), &ProviderRequest{
		Model:    "claude-opus-4-7", // routing-selected model
		Messages: []byte(`[{"role":"user","content":[{"type":"text","text":"Hello"}]}]`),
		MaxTokens: 100,
		Stream:    true,
		APIKey:    "test-key",
		URL:       srv.URL,
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	_ = rc.Close()

	if len(gotBody) == 0 {
		t.Fatal("upstream received no body")
	}
	var upstream struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(gotBody, &upstream); err != nil {
		t.Fatalf("parse upstream body: %v", err)
	}
	if upstream.Model != "claude-opus-4-7" {
		t.Errorf("upstream model = %q, want claude-opus-4-7 (routing-selected)", upstream.Model)
	}
}

func TestAnthropicAdapterURLConstruction(t *testing.T) {
	tests := []struct {
		name    string
		urlIn   func(srvURL string) string
		wantPath string
	}{
		{
			name:    "no trailing slash",
			urlIn:   func(s string) string { return s },
			wantPath: "/v1/messages",
		},
		{
			name:    "trailing slash",
			urlIn:   func(s string) string { return s + "/" },
			wantPath: "/v1/messages",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			a := NewAnthropicAdapter()
			rc, err := a.Stream(context.Background(), &ProviderRequest{
				Model:    "claude-sonnet-4-6",
				Messages: []byte(`[]`),
				Stream:   true,
				APIKey:   "test-key",
				URL:      tt.urlIn(srv.URL),
			})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			_ = rc.Close()

			if gotPath != tt.wantPath {
				t.Errorf("path = %q, want %q", gotPath, tt.wantPath)
			}
		})
	}
}
