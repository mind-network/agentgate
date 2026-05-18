package transformer

import (
	"encoding/json"
	"testing"
)

func TestAnthropicToIR(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hello"}]}
		],
		"tools": [{"name": "read_file", "description": "Read a file", "input_schema": {"type": "object"}}]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}
	if req.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q", req.Model)
	}
	if !req.Stream {
		t.Error("expected stream=true")
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(req.Messages))
	}
	if len(req.Tools) != 1 {
		t.Errorf("expected 1 tool, got %d", len(req.Tools))
	}
}

func TestAnthropicToIRToolUse(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": false,
		"messages": [
			{"role": "assistant", "content": [
				{"type": "text", "text": "Let me read that file"},
				{"type": "tool_use", "id": "tool_001", "name": "read_file", "input": {"path": "/x"}}
			]}
		]
	}`)

	req, _ := AnthropicToIR(body)
	msg := req.Messages[0]
	if len(msg.Content) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(msg.Content))
	}
	if msg.Content[1].Type != "tool_use" {
		t.Errorf("expected tool_use, got %s", msg.Content[1].Type)
	}
	if msg.Content[1].ToolUse.Name != "read_file" {
		t.Errorf("expected read_file, got %s", msg.Content[1].ToolUse.Name)
	}
}

func TestOpenAIToIR(t *testing.T) {
	body := []byte(`{
		"model": "gpt-4o",
		"max_tokens": 500,
		"stream": true,
		"messages": [
			{"role": "user", "content": "Hello"}
		]
	}`)

	req, err := OpenAIToIR(body)
	if err != nil {
		t.Fatalf("OpenAIToIR: %v", err)
	}
	if req.Model != "gpt-4o" {
		t.Errorf("model = %q", req.Model)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(req.Messages))
	}
}

func TestIRToOpenAIRoundTrip(t *testing.T) {
	body := []byte(`{"model":"gpt-4o","max_tokens":500,"stream":true,"messages":[{"role":"user","content":"Hello"}]}`)

	req, err := OpenAIToIR(body)
	if err != nil {
		t.Fatalf("OpenAIToIR: %v", err)
	}

	out, err := IRToOpenAI(req)
	if err != nil {
		t.Fatalf("IRToOpenAI: %v", err)
	}

	var back openAIRequest
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Model != "gpt-4o" {
		t.Errorf("round-trip model = %q", back.Model)
	}
	if len(back.Messages) != 1 {
		t.Errorf("round-trip messages = %d", len(back.Messages))
	}
}

func TestIRToOpenAIStreamOptions(t *testing.T) {
	// Verify that IRToOpenAI includes stream_options.include_usage=true
	// for streaming requests, and omits it for non-streaming requests.
	t.Run("streaming includes stream_options", func(t *testing.T) {
		out, err := IRToOpenAI(&IRRequest{Stream: true, Model: "gpt-4o", Messages: nil})
		if err != nil {
			t.Fatalf("IRToOpenAI: %v", err)
		}
		var parsed struct {
			Stream        bool                 `json:"stream"`
			StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
		}
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !parsed.Stream {
			t.Error("expected stream=true")
		}
		if parsed.StreamOptions == nil {
			t.Fatal("expected stream_options to be present")
		}
		if !parsed.StreamOptions.IncludeUsage {
			t.Error("expected stream_options.include_usage=true")
		}
	})

	t.Run("non-streaming omits stream_options", func(t *testing.T) {
		out, err := IRToOpenAI(&IRRequest{Stream: false, Model: "gpt-4o"})
		if err != nil {
			t.Fatalf("IRToOpenAI: %v", err)
		}
		var parsed struct {
			Stream        bool                 `json:"stream"`
			StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
		}
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if parsed.Stream {
			t.Error("expected stream=false")
		}
		if parsed.StreamOptions != nil {
			t.Error("expected stream_options to be omitted for non-streaming request")
		}
	})
}

func TestStripAnthropicFeaturesNil(t *testing.T) {
	degraded := StripAnthropicFeatures(nil)
	if len(degraded) != 0 {
		t.Errorf("nil request should have no degraded features, got %v", degraded)
	}
}

func TestAnthropicStreamEventToIR(t *testing.T) {
	data := []byte(`{"type":"content_block_delta","delta":{"type":"text_delta","text":"Hello"}}`)

	ev, err := AnthropicStreamEventToIR(data)
	if err != nil {
		t.Fatalf("AnthropicStreamEventToIR: %v", err)
	}
	if ev.Type != "content_block_delta" {
		t.Errorf("type = %q", ev.Type)
	}
	if ev.Delta == nil || ev.Delta.Text != "Hello" {
		t.Error("delta text missing")
	}
}

func TestOpenAIStreamEventToIR(t *testing.T) {
	data := []byte(`{
		"choices": [{"delta": {"content": "Hello world"}}],
		"usage": {"prompt_tokens": 5, "completion_tokens": 2}
	}`)

	ev, err := OpenAIStreamEventToIR(data)
	if err != nil {
		t.Fatalf("OpenAIStreamEventToIR: %v", err)
	}
	if ev.Delta == nil || ev.Delta.Text != "Hello world" {
		t.Errorf("delta text = %q", ev.Delta.Text)
	}
	if ev.Usage == nil || ev.Usage.Input != 5 {
		t.Error("usage missing")
	}
}

func TestAnthropicToIR_StringContent(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "user", "content": "hi"}
		]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(req.Messages))
	}
	msg := req.Messages[0]
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(msg.Content))
	}
	c := msg.Content[0]
	if c.Type != "text" {
		t.Errorf("expected text block, got %q", c.Type)
	}
	if c.Text != "hi" {
		t.Errorf("expected text 'hi', got %q", c.Text)
	}
}

func TestAnthropicToIR_StringSystem(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"system": "You are a helpful assistant",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Hello"}]}]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}
	if len(req.System) != 1 {
		t.Fatalf("expected 1 system block, got %d", len(req.System))
	}
	s := req.System[0]
	if s.Type != "text" {
		t.Errorf("expected text block, got %q", s.Type)
	}
	if s.Text != "You are a helpful assistant" {
		t.Errorf("expected system text, got %q", s.Text)
	}
}

func TestAnthropicToIR_MixedContentForms(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "user", "content": "string form"},
			{"role": "assistant", "content": [
				{"type": "text", "text": "array form"},
				{"type": "tool_use", "id": "tool_001", "name": "read_file", "input": {"path": "/x"}}
			]}
		]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(req.Messages))
	}
	// First message: string form → one text block
	m0 := req.Messages[0]
	if len(m0.Content) != 1 || m0.Content[0].Type != "text" || m0.Content[0].Text != "string form" {
		t.Errorf("msg[0]: expected single text block 'string form', got %+v", m0.Content)
	}
	// Second message: array form → two blocks
	m1 := req.Messages[1]
	if len(m1.Content) != 2 {
		t.Fatalf("msg[1]: expected 2 blocks, got %d", len(m1.Content))
	}
	if m1.Content[0].Type != "text" || m1.Content[0].Text != "array form" {
		t.Errorf("msg[1] block 0: expected text 'array form', got %+v", m1.Content[0])
	}
	if m1.Content[1].Type != "tool_use" {
		t.Errorf("msg[1] block 1: expected tool_use, got %q", m1.Content[1].Type)
	}
}

func TestAnthropicToIR_ArrayContentStillWorks(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hello"}, {"type": "text", "text": "World"}]}
		],
		"system": [{"type": "text", "text": "System prompt", "cache_control": {"type": "ephemeral"}}]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(req.Messages))
	}
	msg := req.Messages[0]
	if len(msg.Content) != 2 {
		t.Fatalf("expected 2 content blocks, got %d", len(msg.Content))
	}
	if msg.Content[0].Text != "Hello" {
		t.Errorf("block 0 text = %q", msg.Content[0].Text)
	}
	if msg.Content[1].Text != "World" {
		t.Errorf("block 1 text = %q", msg.Content[1].Text)
	}
	if len(req.System) != 1 || req.System[0].Text != "System prompt" {
		t.Errorf("system = %+v", req.System)
	}
	if req.System[0].CacheControl == nil {
		t.Error("expected cache_control on system block")
	}
}

func TestAnthropicToIR_MalformedContent(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "user", "content": 123}
		]
	}`)

	_, err := AnthropicToIR(body)
	if err == nil {
		t.Fatal("expected error for numeric content, got nil")
	}
}

func TestAnthropicToIRThinking(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"thinking": {"type": "enabled", "budget_tokens": 16000},
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Hi"}]}]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}
	if req.Thinking == nil {
		t.Fatal("expected thinking spec")
	}
	if req.Thinking.BudgetTokens != 16000 {
		t.Errorf("thinking budget = %d", req.Thinking.BudgetTokens)
	}
}

func TestIRToAnthropicMessagesRoundTrip(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "user", "content": [{"type": "text", "text": "Hello", "cache_control": {"type": "ephemeral"}}]},
			{"role": "assistant", "content": [
				{"type": "thinking", "thinking": "Let me think about this"},
				{"type": "text", "text": "Let me check"},
				{"type": "tool_use", "id": "tu_1", "name": "read_file", "input": {"path": "/x"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tu_1", "content": [
					{"type": "text", "text": "line 1", "cache_control": {"type": "ephemeral"}},
					{"type": "text", "text": "line 2"}
				]}
			]}
		]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}

	out, err := IRToAnthropicMessages(req)
	if err != nil {
		t.Fatalf("IRToAnthropicMessages: %v", err)
	}

	// Parse output back through AnthropicToIR
	wrapper := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"stream":true,"messages":`)
	wrapper = append(wrapper, out...)
	wrapper = append(wrapper, []byte(`}`)...)

	req2, err := AnthropicToIR(wrapper)
	if err != nil {
		t.Fatalf("AnthropicToIR on round-trip output: %v (output was: %s)", err, string(out))
	}

	if len(req2.Messages) != 3 {
		t.Fatalf("round-trip: expected 3 messages, got %d", len(req2.Messages))
	}
	// Verify text block with cache_control survived
	m0 := req2.Messages[0]
	if m0.Content[0].CacheControl == nil || m0.Content[0].CacheControl.Type != "ephemeral" {
		t.Error("round-trip: cache_control on text block lost")
	}
	// Verify thinking + text + tool_use survived
	m1 := req2.Messages[1]
	if len(m1.Content) != 3 {
		t.Fatalf("round-trip msg[1]: expected 3 blocks, got %d", len(m1.Content))
	}
	if m1.Content[0].Type != "thinking" || m1.Content[0].Thinking == nil || m1.Content[0].Thinking.Text != "Let me think about this" {
		t.Errorf("round-trip msg[1] thinking: %+v", m1.Content[0])
	}
	if m1.Content[2].Type != "tool_use" || m1.Content[2].ToolUse.ID != "tu_1" {
		t.Errorf("round-trip msg[1] tool_use: %+v", m1.Content[2])
	}
	// Verify tool_result with nested cache_control survived
	m2 := req2.Messages[2]
	if m2.Content[0].Type != "tool_result" || m2.Content[0].ToolResult.ToolUseID != "tu_1" {
		t.Errorf("round-trip msg[2] tool_result: %+v", m2.Content[0])
	}
	tr := m2.Content[0].ToolResult
	if len(tr.Content) != 2 {
		t.Fatalf("round-trip tool_result.content: expected 2 blocks, got %d", len(tr.Content))
	}
	if tr.Content[0].Text != "line 1" {
		t.Errorf("round-trip tool_result.content[0] text = %q", tr.Content[0].Text)
	}
	if tr.Content[0].CacheControl == nil || tr.Content[0].CacheControl.Type != "ephemeral" {
		t.Error("round-trip: nested cache_control inside tool_result.content lost")
	}
	if tr.Content[1].Text != "line 2" {
		t.Errorf("round-trip tool_result.content[1] text = %q", tr.Content[1].Text)
	}
}

func TestIRToAnthropicMessagesNestedCacheControl(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tu_1", "content": [
					{"type": "text", "text": "cached block", "cache_control": {"type": "ephemeral"}},
					{"type": "text", "text": "non-cached block"}
				]}
			]}
		]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}
	out, err := IRToAnthropicMessages(req)
	if err != nil {
		t.Fatalf("IRToAnthropicMessages: %v", err)
	}

	// Parse the output and find the nested blocks.
	var msgs []json.RawMessage
	if err := json.Unmarshal(out, &msgs); err != nil {
		t.Fatalf("output not a JSON array: %v", err)
	}
	var parsed struct {
		Content []struct {
			Content []struct {
				Text         string `json:"text"`
				CacheControl *struct {
					Type string `json:"type"`
				} `json:"cache_control"`
			} `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msgs[0], &parsed); err != nil {
		t.Fatalf("parse message: %v", err)
	}
	if len(parsed.Content) != 1 {
		t.Fatalf("expected 1 top-level block, got %d", len(parsed.Content))
	}
	nested := parsed.Content[0].Content
	if len(nested) != 2 {
		t.Fatalf("expected 2 nested blocks, got %d", len(nested))
	}
	if nested[0].CacheControl == nil || nested[0].CacheControl.Type != "ephemeral" {
		t.Errorf("nested cache_control lost: %+v", nested[0])
	}
	if nested[1].CacheControl != nil {
		t.Errorf("second nested block should have no cache_control: %+v", nested[1])
	}
}

func TestIRToAnthropicMessagesEmpty(t *testing.T) {
	out, err := IRToAnthropicMessages(nil)
	if err != nil {
		t.Fatalf("IRToAnthropicMessages(nil): %v", err)
	}
	if string(out) != "[]" {
		t.Errorf("nil request: expected [], got %s", string(out))
	}

	out, err = IRToAnthropicMessages(&IRRequest{})
	if err != nil {
		t.Fatalf("IRToAnthropicMessages(empty): %v", err)
	}
	if string(out) != "[]" {
		t.Errorf("empty messages: expected [], got %s", string(out))
	}
}

func TestIRToAnthropicMessagesStringInputNormalises(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "user", "content": "plain string"}
		]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}

	out, err := IRToAnthropicMessages(req)
	if err != nil {
		t.Fatalf("IRToAnthropicMessages: %v", err)
	}

	// The output must be a JSON array of messages, with content as array-of-blocks.
	var msgs []json.RawMessage
	if err := json.Unmarshal(out, &msgs); err != nil {
		t.Fatalf("output is not a valid JSON array: %v (got: %s)", err, string(out))
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	// Parse the single message and check content is an array.
	var parsed struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(msgs[0], &parsed); err != nil {
		t.Fatalf("failed to parse message: %v", err)
	}
	if parsed.Role != "user" {
		t.Errorf("role = %q, want user", parsed.Role)
	}
	if len(parsed.Content) != 1 {
		t.Fatalf("expected content array of 1 block, got %d", len(parsed.Content))
	}
	var block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(parsed.Content[0], &block); err != nil {
		t.Fatalf("failed to parse content block: %v", err)
	}
	if block.Type != "text" || block.Text != "plain string" {
		t.Errorf("block = {type:%q text:%q}, want {type:text text:plain string}", block.Type, block.Text)
	}
}

// --- IR → Anthropic top-level re-marshalling tests (HANDOFF-014 T1) ---

func TestIRToAnthropicSystem_StringSystem(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"system": "You are a helpful assistant",
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Hello"}]}]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}

	out, err := IRToAnthropicSystem(req)
	if err != nil {
		t.Fatalf("IRToAnthropicSystem: %v", err)
	}

	var blocks []json.RawMessage
	if err := json.Unmarshal(out, &blocks); err != nil {
		t.Fatalf("output not a JSON array: %v (got: %s)", err, string(out))
	}
	if len(blocks) != 1 {
		t.Fatalf("expected 1 system block, got %d", len(blocks))
	}
	var block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	_ = json.Unmarshal(blocks[0], &block)
	if block.Type != "text" || block.Text != "You are a helpful assistant" {
		t.Errorf("block = {type:%q text:%q}", block.Type, block.Text)
	}
}

func TestIRToAnthropicSystem_ArraySystem(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"system": [{"type": "text", "text": "System prompt", "cache_control": {"type": "ephemeral"}}],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Hello"}]}]
	}`)

	req, _ := AnthropicToIR(body)

	out, err := IRToAnthropicSystem(req)
	if err != nil {
		t.Fatalf("IRToAnthropicSystem: %v", err)
	}

	var blocks []struct {
		Type         string `json:"type"`
		Text         string `json:"text"`
		CacheControl *struct {
			Type string `json:"type"`
		} `json:"cache_control"`
	}
	_ = json.Unmarshal(out, &blocks)
	if len(blocks) != 1 {
		t.Fatalf("expected 1 block, got %d", len(blocks))
	}
	if blocks[0].Text != "System prompt" {
		t.Errorf("text = %q", blocks[0].Text)
	}
	if blocks[0].CacheControl == nil || blocks[0].CacheControl.Type != "ephemeral" {
		t.Errorf("cache_control lost: %+v", blocks[0].CacheControl)
	}
}

func TestIRToAnthropicSystem_Nil(t *testing.T) {
	out, err := IRToAnthropicSystem(nil)
	if err != nil {
		t.Fatalf("IRToAnthropicSystem(nil): %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for nil request, got %s", string(out))
	}

	out, err = IRToAnthropicSystem(&IRRequest{})
	if err != nil {
		t.Fatalf("IRToAnthropicSystem(empty): %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for empty system, got %s", string(out))
	}
}

func TestIRToAnthropicTools_WithSchemas(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"tools": [
			{"name": "read_file", "description": "Read a file", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}}}},
			{"name": "write_file", "description": "Write a file", "input_schema": {"type": "object", "properties": {"path": {"type": "string"}, "content": {"type": "string"}}}}
		],
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Hello"}]}]
	}`)

	req, _ := AnthropicToIR(body)

	out, err := IRToAnthropicTools(req)
	if err != nil {
		t.Fatalf("IRToAnthropicTools: %v", err)
	}

	var tools []struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"input_schema"`
	}
	_ = json.Unmarshal(out, &tools)
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}
	if tools[0].Name != "read_file" {
		t.Errorf("tool[0].name = %q", tools[0].Name)
	}
	if tools[1].Name != "write_file" {
		t.Errorf("tool[1].name = %q", tools[1].Name)
	}
	props0 := tools[0].InputSchema["properties"].(map[string]any)
	if props0["path"].(map[string]any)["type"] != "string" {
		t.Errorf("tool[0] schema path type missing")
	}
}

func TestIRToAnthropicTools_Nil(t *testing.T) {
	out, err := IRToAnthropicTools(nil)
	if err != nil {
		t.Fatalf("IRToAnthropicTools(nil): %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for nil request, got %s", string(out))
	}

	out, err = IRToAnthropicTools(&IRRequest{})
	if err != nil {
		t.Fatalf("IRToAnthropicTools(empty): %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for empty tools, got %s", string(out))
	}
}

func TestIRToAnthropicToolChoice_AutoAnySpecific(t *testing.T) {
	tests := []struct {
		name     string
		jsonBody string
		wantType string
		wantName string
	}{
		{
			name:     "auto",
			jsonBody: `{"model":"c","max_tokens":1,"stream":false,"tool_choice":{"type":"auto"},"messages":[{"role":"user","content":"hi"}]}`,
			wantType: "auto",
		},
		{
			name:     "any",
			jsonBody: `{"model":"c","max_tokens":1,"stream":false,"tool_choice":{"type":"any"},"messages":[{"role":"user","content":"hi"}]}`,
			wantType: "any",
		},
		{
			name:     "specific",
			jsonBody: `{"model":"c","max_tokens":1,"stream":false,"tool_choice":{"type":"specific","name":"read_file"},"messages":[{"role":"user","content":"hi"}]}`,
			wantType: "tool",
			wantName: "read_file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := AnthropicToIR([]byte(tt.jsonBody))
			if err != nil {
				t.Fatalf("AnthropicToIR: %v", err)
			}

			out, err := IRToAnthropicToolChoice(req)
			if err != nil {
				t.Fatalf("IRToAnthropicToolChoice: %v", err)
			}

			var tc struct {
				Type string `json:"type"`
				Name string `json:"name,omitempty"`
			}
			_ = json.Unmarshal(out, &tc)
			if tc.Type != tt.wantType {
				t.Errorf("type = %q, want %q", tc.Type, tt.wantType)
			}
			if tc.Name != tt.wantName {
				t.Errorf("name = %q, want %q", tc.Name, tt.wantName)
			}
		})
	}
}

func TestIRToAnthropicToolChoice_Nil(t *testing.T) {
	out, err := IRToAnthropicToolChoice(nil)
	if err != nil {
		t.Fatalf("IRToAnthropicToolChoice(nil): %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for nil request, got %s", string(out))
	}

	body := []byte(`{"model":"c","max_tokens":1,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := AnthropicToIR(body)
	out, err = IRToAnthropicToolChoice(req)
	if err != nil {
		t.Fatalf("IRToAnthropicToolChoice(no tool_choice): %v", err)
	}
	if out != nil {
		t.Errorf("expected nil when tool_choice absent, got %s", string(out))
	}
}

func TestAnthropicToIR_ForcedToolWireNormalizes(t *testing.T) {
	// Actual Anthropic wire uses "tool" for specific tool selection.
	// The parser must normalize this into IR ToolChoiceSpecific.
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": false,
		"tool_choice": {"type": "tool", "name": "write_file"},
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Write x"}]}]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}
	if req.ToolChoice == nil {
		t.Fatal("expected tool_choice, got nil")
	}
	if req.ToolChoice.Type != "specific" {
		t.Errorf("expected IR type 'specific', got %q", req.ToolChoice.Type)
	}
	if req.ToolChoice.Name != "write_file" {
		t.Errorf("expected name 'write_file', got %q", req.ToolChoice.Name)
	}

	// Round-trip: IR → Anthropic wire should emit "tool".
	out, err := IRToAnthropicToolChoice(req)
	if err != nil {
		t.Fatalf("IRToAnthropicToolChoice: %v", err)
	}
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
	}
	_ = json.Unmarshal(out, &tc)
	if tc.Type != "tool" {
		t.Errorf("round-trip: expected Anthropic wire type 'tool', got %q", tc.Type)
	}
	if tc.Name != "write_file" {
		t.Errorf("round-trip: expected name 'write_file', got %q", tc.Name)
	}
}

func TestAnthropicToIR_MetadataPassthrough(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"metadata": {"user_id": "user-123", "session": "abc"},
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Hello"}]}]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}
	if req.Metadata == nil {
		t.Fatal("expected metadata, got nil")
	}
	if req.Metadata["user_id"] != "user-123" {
		t.Errorf("metadata user_id = %q", req.Metadata["user_id"])
	}
	if req.Metadata["session"] != "abc" {
		t.Errorf("metadata session = %q", req.Metadata["session"])
	}
}

func TestIRToAnthropicThinking_RoundTrip(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"thinking": {"type": "enabled", "budget_tokens": 16000},
		"messages": [{"role": "user", "content": [{"type": "text", "text": "Hi"}]}]
	}`)

	req, _ := AnthropicToIR(body)

	out, err := IRToAnthropicThinking(req)
	if err != nil {
		t.Fatalf("IRToAnthropicThinking: %v", err)
	}

	var th struct {
		Type         string `json:"type"`
		BudgetTokens int    `json:"budget_tokens"`
	}
	_ = json.Unmarshal(out, &th)
	if th.Type != "enabled" {
		t.Errorf("type = %q, want enabled", th.Type)
	}
	if th.BudgetTokens != 16000 {
		t.Errorf("budget_tokens = %d", th.BudgetTokens)
	}
}

// --- T2: nested tool_result.content string shorthand ---

func TestAnthropicToIR_NestedToolResultStringContent(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tool_001", "content": "file contents here"}
			]}
		]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}
	if len(req.Messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(req.Messages))
	}
	msg := req.Messages[0]
	if len(msg.Content) != 1 {
		t.Fatalf("expected 1 top-level block, got %d", len(msg.Content))
	}
	cb := msg.Content[0]
	if cb.Type != "tool_result" {
		t.Fatalf("expected tool_result block, got %q", cb.Type)
	}
	if cb.ToolResult == nil {
		t.Fatal("expected ToolResult, got nil")
	}
	if cb.ToolResult.ToolUseID != "tool_001" {
		t.Errorf("ToolUseID = %q, want tool_001", cb.ToolResult.ToolUseID)
	}
	if len(cb.ToolResult.Content) != 1 {
		t.Fatalf("expected 1 nested content block, got %d", len(cb.ToolResult.Content))
	}
	nested := cb.ToolResult.Content[0]
	if nested.Type != "text" {
		t.Errorf("nested block type = %q, want text", nested.Type)
	}
	if nested.Text != "file contents here" {
		t.Errorf("nested block text = %q, want 'file contents here'", nested.Text)
	}
}

func TestAnthropicToIR_NestedToolResultRoundTrip(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tool_001", "content": "result text"}
			]}
		]
	}`)

	req, err := AnthropicToIR(body)
	if err != nil {
		t.Fatalf("AnthropicToIR: %v", err)
	}

	out, err := IRToAnthropicMessages(req)
	if err != nil {
		t.Fatalf("IRToAnthropicMessages: %v", err)
	}

	// Parse output and verify nested content is a JSON array of blocks, not a string.
	var msgs []json.RawMessage
	if err := json.Unmarshal(out, &msgs); err != nil {
		t.Fatalf("output not a JSON array: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}

	var parsed struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
		} `json:"content"`
	}
	if err := json.Unmarshal(msgs[0], &parsed); err != nil {
		t.Fatalf("parse message: %v", err)
	}
	if len(parsed.Content) != 1 {
		t.Fatalf("expected 1 top-level block, got %d", len(parsed.Content))
	}
	trBlock := parsed.Content[0]
	if trBlock.Type != "tool_result" {
		t.Errorf("type = %q, want tool_result", trBlock.Type)
	}

	// Nested content must be a JSON array, not a string.
	var nestedBlocks []json.RawMessage
	if err := json.Unmarshal(trBlock.Content, &nestedBlocks); err != nil {
		t.Fatalf("nested content is not a JSON array: %v (raw: %s)", err, string(trBlock.Content))
	}
	if len(nestedBlocks) != 1 {
		t.Fatalf("expected 1 nested block, got %d", len(nestedBlocks))
	}
	var nestedBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(nestedBlocks[0], &nestedBlock); err != nil {
		t.Fatalf("parse nested block: %v", err)
	}
	if nestedBlock.Type != "text" || nestedBlock.Text != "result text" {
		t.Errorf("nested block = {type:%q text:%q}, want {type:text text:'result text'}", nestedBlock.Type, nestedBlock.Text)
	}
}

func TestAnthropicToIR_NestedMalformedContent(t *testing.T) {
	body := []byte(`{
		"model": "claude-sonnet-4-6",
		"max_tokens": 1024,
		"stream": true,
		"messages": [
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "tool_001", "content": 123}
			]}
		]
	}`)

	_, err := AnthropicToIR(body)
	if err == nil {
		t.Fatal("expected error for numeric nested content, got nil")
	}
}

func TestIRToAnthropicThinking_Nil(t *testing.T) {
	out, err := IRToAnthropicThinking(nil)
	if err != nil {
		t.Fatalf("IRToAnthropicThinking(nil): %v", err)
	}
	if out != nil {
		t.Errorf("expected nil for nil request, got %s", string(out))
	}

	body := []byte(`{"model":"c","max_tokens":1,"stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := AnthropicToIR(body)
	out, err = IRToAnthropicThinking(req)
	if err != nil {
		t.Fatalf("IRToAnthropicThinking(no thinking): %v", err)
	}
	if out != nil {
		t.Errorf("expected nil when thinking absent, got %s", string(out))
	}
}
