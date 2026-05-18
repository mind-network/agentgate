// Package upstream_mock provides a minimal SSE-emitting HTTP server
// that mimics the Anthropic Messages API for conformance testing.
package upstream_mock

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
)

// Server is a mock Anthropic Messages API server.
type Server struct {
	*httptest.Server
	mu        sync.Mutex
	Requests  []CapturedRequest
	LatencyMs int // artificial delay per event
}

// CapturedRequest records a request received by the mock server.
type CapturedRequest struct {
	Model       string
	Stream      bool
	Messages    []map[string]any
	MaxTokens   int
	Temperature *float64
	System      any              `json:"system,omitempty"`
	Tools       []map[string]any `json:"tools,omitempty"`
	ToolChoice  any              `json:"tool_choice,omitempty"`
	Metadata    map[string]any   `json:"metadata,omitempty"`
	Thinking    any              `json:"thinking,omitempty"`
}

// NewServer creates a new mock upstream server.
func NewServer() *Server {
	s := &Server{}
	s.Server = httptest.NewServer(http.HandlerFunc(s.handle))
	return s
}

// Reset clears captured requests.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Requests = nil
}

// RequestCount returns the number of captured requests.
func (s *Server) RequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.Requests)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("x-api-key") == "" && r.Header.Get("Authorization") == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	var body struct {
		Model       string           `json:"model"`
		Stream      bool             `json:"stream"`
		Messages    []map[string]any `json:"messages"`
		MaxTokens   int              `json:"max_tokens"`
		Temperature *float64         `json:"temperature"`
		System      json.RawMessage  `json:"system,omitempty"`
		Tools       []map[string]any `json:"tools,omitempty"`
		ToolChoice  json.RawMessage  `json:"tool_choice,omitempty"`
		Metadata    map[string]any   `json:"metadata,omitempty"`
		Thinking    json.RawMessage  `json:"thinking,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	s.Requests = append(s.Requests, CapturedRequest{
		Model:       body.Model,
		Stream:      body.Stream,
		Messages:    body.Messages,
		MaxTokens:   body.MaxTokens,
		Temperature: body.Temperature,
		System:      body.System,
		Tools:       body.Tools,
		ToolChoice:  body.ToolChoice,
		Metadata:    body.Metadata,
		Thinking:    body.Thinking,
	})
	s.mu.Unlock()

	if body.Stream {
		s.handleStream(w, body.Model)
	} else {
		s.handleNonStream(w, body.Model)
	}
}

func (s *Server) handleStream(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	flusher, _ := w.(http.Flusher)

	events := []string{
		`event: message_start
data: {"type":"message_start","message":{"id":"msg_001","type":"message","role":"assistant","model":"` + model + `","content":[],"usage":null}}`,

		`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,

		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Sure, let me help with that code change."}}`,

		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"\n\nHere is the updated implementation:\n\n` + "```go" + `\nfunc main() {\n    fmt.Println(\"hello\")\n}\n` + "```" + `"}}`,

		`event: content_block_stop
data: {"type":"content_block_stop","index":0}`,

		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":128}}`,

		`event: message_stop
data: {"type":"message_stop"}`,
	}

	for _, ev := range events {
		_, _ = fmt.Fprint(w, ev+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

func (s *Server) handleNonStream(w http.ResponseWriter, model string) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{
		"id":    "msg_001",
		"type":  "message",
		"role":  "assistant",
		"model": model,
		"content": []map[string]any{
			{"type": "text", "text": "Here is the code change."},
		},
		"usage": map[string]int{
			"input_tokens":  256,
			"output_tokens": 128,
		},
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// SSEEventCount returns the number of SSE events in the mock response
// (used by conformance assertions).
func SSEEventCount() int { return 7 }
