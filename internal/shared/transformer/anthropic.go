// Package transformer converts between IR and upstream provider wire formats.
// No anthropic-sdk-go types appear here — all conversions use plain JSON/structs.
package transformer

import (
	"encoding/json"

	"agentgate/internal/shared/ir"
)

// anthropicContent is a slice of anthropicBlock that accepts JSON string shorthand.
// Anthropic's API allows content/system to be either a single string or an array of blocks.
type anthropicContent []anthropicBlock

func (c *anthropicContent) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*c = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*c = []anthropicBlock{{Type: "text", Text: s}}
		return nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(b, &blocks); err != nil {
		return err
	}
	*c = blocks
	return nil
}

// AnthropicToIR converts an Anthropic Messages API request body into IR types.
// Input is raw JSON bytes (the original wire body).
func AnthropicToIR(body []byte) (*IRRequest, error) {
	var raw anthropicRequest
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	req := &IRRequest{
		Model:    raw.Model,
		Stream:   raw.Stream,
		MaxTokens: raw.MaxTokens,
		Metadata: raw.Metadata,
	}
	if raw.Temperature != nil {
		req.Temperature = raw.Temperature
	}
	if raw.Thinking != nil {
		req.Thinking = &ir.ThinkingSpec{
			Enabled:      true,
			BudgetTokens: raw.Thinking.BudgetTokens,
		}
	}

	for _, msg := range raw.Messages {
		m := ir.Message{Role: ir.Role(msg.Role)}
		for _, c := range msg.Content {
			m.Content = append(m.Content, anthropicBlockToIR(c))
		}
		req.Messages = append(req.Messages, m)
	}

	if raw.System != nil {
		for _, c := range raw.System {
			req.System = append(req.System, anthropicBlockToIR(c))
		}
	}

	for _, t := range raw.Tools {
		req.Tools = append(req.Tools, ir.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	if raw.ToolChoice != nil {
		tcType := ir.ToolChoiceType(raw.ToolChoice.Type)
		// Anthropic wire uses "tool" for specific tool selection; normalize to IR.
		if tcType == "tool" {
			tcType = ir.ToolChoiceSpecific
		}
		req.ToolChoice = &ir.ToolChoice{
			Type: tcType,
			Name: raw.ToolChoice.Name,
		}
	}
	return req, nil
}

// IRToAnthropicSystem converts IR system blocks into an Anthropic-shaped JSON array.
// Returns nil when there are no system blocks (field should be omitted).
func IRToAnthropicSystem(req *IRRequest) ([]byte, error) {
	if req == nil || len(req.System) == 0 {
		return nil, nil
	}
	var blocks []anthropicBlock
	for _, c := range req.System {
		blocks = append(blocks, irBlockToAnthropicBlock(c))
	}
	return json.Marshal(blocks)
}

// IRToAnthropicTools converts IR tools into an Anthropic-shaped JSON array.
// Returns nil when there are no tools (field should be omitted).
func IRToAnthropicTools(req *IRRequest) ([]byte, error) {
	if req == nil || len(req.Tools) == 0 {
		return nil, nil
	}
	var tools []anthropicTool
	for _, t := range req.Tools {
		tools = append(tools, anthropicTool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return json.Marshal(tools)
}

// IRToAnthropicToolChoice converts an IR ToolChoice into an Anthropic-shaped JSON object.
// IR "specific" is emitted as Anthropic "tool"; "auto" and "any" pass through.
// Returns nil when tool_choice is nil (field should be omitted).
func IRToAnthropicToolChoice(req *IRRequest) ([]byte, error) {
	if req == nil || req.ToolChoice == nil {
		return nil, nil
	}
	tc := anthropicToolChoice{Type: string(req.ToolChoice.Type)}
	if req.ToolChoice.Type == ir.ToolChoiceSpecific {
		tc.Type = "tool"
	}
	if req.ToolChoice.Name != "" {
		tc.Name = req.ToolChoice.Name
	}
	return json.Marshal(tc)
}

// IRToAnthropicThinking converts an IR ThinkingSpec into an Anthropic-shaped JSON object.
// Returns nil when thinking is nil (field should be omitted).
func IRToAnthropicThinking(req *IRRequest) ([]byte, error) {
	if req == nil || req.Thinking == nil {
		return nil, nil
	}
	return json.Marshal(anthropicThinking{
		Type:         "enabled",
		BudgetTokens: req.Thinking.BudgetTokens,
	})
}

// IRToAnthropicMessages converts IR messages into a JSON array of Anthropic-shaped message objects.
func IRToAnthropicMessages(req *IRRequest) ([]byte, error) {
	if req == nil || len(req.Messages) == 0 {
		return []byte("[]"), nil
	}
	var out []anthropicMessage
	for _, msg := range req.Messages {
		am := anthropicMessage{Role: string(msg.Role)}
		for _, c := range msg.Content {
			am.Content = append(am.Content, irBlockToAnthropicBlock(c))
		}
		out = append(out, am)
	}
	return json.Marshal(out)
}

// AnthropicStreamEventToIR converts a raw SSE data line into an IRStreamEvent.
func AnthropicStreamEventToIR(data []byte) (*IRStreamEvent, error) {
	var raw anthropicSSEEvent
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	ev := &IRStreamEvent{Raw: data, Type: ir.StreamEventType(raw.Type)}
	if raw.Usage != nil {
		ev.Usage = &ir.Usage{
			Input:       raw.Usage.InputTokens,
			Output:      raw.Usage.OutputTokens,
			CacheRead:   raw.Usage.CacheReadInputTokens,
			CacheCreate: raw.Usage.CacheCreationInputTokens,
		}
	}
	if raw.Delta != nil {
		ev.Delta = &ir.ContentBlock{
			Type: ir.BlockText,
			Text: raw.Delta.Text,
		}
	}
	return ev, nil
}

// anthropic wire shapes (pure structs, no SDK dependency)
type anthropicRequest struct {
	Model       string              `json:"model"`
	Messages    []anthropicMessage  `json:"messages"`
	System      anthropicContent   `json:"system,omitempty"`
	Tools       []anthropicTool     `json:"tools,omitempty"`
	ToolChoice  *anthropicToolChoice `json:"tool_choice,omitempty"`
	MaxTokens   int                 `json:"max_tokens"`
	Temperature *float64            `json:"temperature,omitempty"`
	Stream      bool                `json:"stream"`
	Thinking    *anthropicThinking  `json:"thinking,omitempty"`
	Metadata    map[string]string   `json:"metadata,omitempty"`
}

type anthropicMessage struct {
	Role    string            `json:"role"`
	Content anthropicContent `json:"content"`
}

type anthropicBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      anthropicContent `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	CacheControl *struct {
		Type string `json:"type"`
	} `json:"cache_control,omitempty"`
	Thinking *string `json:"thinking,omitempty"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicSSEEvent struct {
	Type  string        `json:"type"`
	Delta *struct {
		Text string `json:"text,omitempty"`
		Type string `json:"type,omitempty"`
	} `json:"delta,omitempty"`
	Usage *struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage,omitempty"`
}

// IRRequest is the IR-level request used by adapters.
type IRRequest struct {
	Model       string
	Messages    []ir.Message
	System      []ir.ContentBlock
	Tools       []ir.Tool
	ToolChoice  *ir.ToolChoice
	MaxTokens   int
	Temperature *float64
	Stream      bool
	Thinking    *ir.ThinkingSpec
	Metadata    map[string]string
}

// IRStreamEvent is an IR-level streaming event.
type IRStreamEvent struct {
	Type  ir.StreamEventType
	Raw   []byte
	Delta *ir.ContentBlock
	Usage *ir.Usage
}

func anthropicBlockToIR(c anthropicBlock) ir.ContentBlock {
	cb := ir.ContentBlock{Type: ir.BlockType(c.Type)}
	switch c.Type {
	case "text":
		cb.Text = c.Text
	case "tool_use":
		cb.ToolUse = &ir.ToolUse{ID: c.ID, Name: c.Name, Input: parseJSONMap(c.Input)}
	case "tool_result":
		var content []ir.ContentBlock
		for _, cc := range c.Content {
			content = append(content, anthropicBlockToIR(cc))
		}
		cb.ToolResult = &ir.ToolResult{ToolUseID: c.ToolUseID, Content: content, IsError: c.IsError}
	case "thinking":
		text := ""
		if c.Thinking != nil {
			text = *c.Thinking
		}
		cb.Thinking = &ir.ThinkingBlock{Text: text}
	}
	if c.CacheControl != nil {
		cb.CacheControl = &ir.CacheControlSpec{Type: c.CacheControl.Type}
	}
	return cb
}

func irBlockToAnthropicBlock(cb ir.ContentBlock) anthropicBlock {
	ab := anthropicBlock{Type: string(cb.Type)}
	switch cb.Type {
	case ir.BlockText:
		ab.Text = cb.Text
	case ir.BlockToolUse:
		if cb.ToolUse != nil {
			ab.ID = cb.ToolUse.ID
			ab.Name = cb.ToolUse.Name
			b, _ := json.Marshal(cb.ToolUse.Input)
			ab.Input = b
		}
	case ir.BlockToolResult:
		if cb.ToolResult != nil {
			ab.ToolUseID = cb.ToolResult.ToolUseID
			ab.IsError = cb.ToolResult.IsError
			for _, cc := range cb.ToolResult.Content {
				ab.Content = append(ab.Content, irBlockToAnthropicBlock(cc))
			}
		}
	case ir.BlockThinking:
		if cb.Thinking != nil {
			ab.Thinking = &cb.Thinking.Text
		}
	}
	if cb.CacheControl != nil {
		ab.CacheControl = &struct {
			Type string `json:"type"`
		}{Type: cb.CacheControl.Type}
	}
	return ab
}

func parseJSONMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}
