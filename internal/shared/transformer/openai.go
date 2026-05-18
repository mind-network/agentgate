package transformer

import (
	"encoding/json"

	"agentgate/internal/shared/ir"
)

// OpenAIToIR converts an OpenAI Chat Completions request body into IR types.
func OpenAIToIR(body []byte) (*IRRequest, error) {
	var raw openAIRequest
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}

	req := &IRRequest{
		Model:     raw.Model,
		Stream:    raw.Stream,
		MaxTokens: raw.MaxTokens,
	}
	if raw.Temperature != nil {
		req.Temperature = raw.Temperature
	}

	for _, msg := range raw.Messages {
		m := ir.Message{Role: openAIRoleToIR(msg.Role)}
		switch msg.Role {
		case "assistant":
			if msg.Content != nil {
				m.Content = append(m.Content, ir.ContentBlock{Type: ir.BlockText, Text: *msg.Content})
			}
			for _, tc := range msg.ToolCalls {
				m.Content = append(m.Content, ir.ContentBlock{
					Type: ir.BlockToolUse,
					ToolUse: &ir.ToolUse{
						ID:    tc.ID,
						Name:  tc.Function.Name,
						Input: parseJSONString(tc.Function.Arguments),
					},
				})
			}
		case "user":
			if msg.Content != nil {
				m.Content = append(m.Content, ir.ContentBlock{Type: ir.BlockText, Text: *msg.Content})
			}
		case "tool":
			m.Content = append(m.Content, ir.ContentBlock{
				Type: ir.BlockToolResult,
				ToolResult: &ir.ToolResult{
					ToolUseID: msg.ToolCallID,
					Content:   []ir.ContentBlock{{Type: ir.BlockText, Text: ptrStr(msg.Content)}},
				},
			})
		}
		req.Messages = append(req.Messages, m)
	}

	for _, t := range raw.Tools {
		req.Tools = append(req.Tools, ir.Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	return req, nil
}

// IRToOpenAI converts an IR request to an OpenAI Chat Completions request body.
func IRToOpenAI(req *IRRequest) ([]byte, error) {
	var out openAIRequest
	out.Model = req.Model
	out.Stream = req.Stream
	out.MaxTokens = req.MaxTokens
	out.Temperature = req.Temperature

	if req.Stream {
		out.StreamOptions = &openAIStreamOptions{IncludeUsage: true}
	}

	for _, msg := range req.Messages {
		om := openAIMessage{Role: irRoleToOpenAI(msg.Role)}
		for _, c := range msg.Content {
			switch c.Type {
			case ir.BlockText:
				om.Content = &c.Text
			case ir.BlockToolUse:
				args, _ := json.Marshal(c.ToolUse.Input)
				om.ToolCalls = append(om.ToolCalls, openAIToolCall{
					ID:   c.ToolUse.ID,
					Type: "function",
					Function: openAIFunctionCall{
						Name:      c.ToolUse.Name,
						Arguments: string(args),
					},
				})
			case ir.BlockToolResult:
				text := ""
				if len(c.ToolResult.Content) > 0 {
					text = c.ToolResult.Content[0].Text
				}
				om.Content = &text
				om.ToolCallID = c.ToolResult.ToolUseID
			}
		}
		out.Messages = append(out.Messages, om)
	}

	for _, t := range req.Tools {
		out.Tools = append(out.Tools, openAITool{
			Type: "function",
			Function: openAIToolFunc{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.InputSchema,
			},
		})
	}
	return json.Marshal(out)
}

// OpenAIStreamEventToIR converts a raw OpenAI SSE data line into an IRStreamEvent.
func OpenAIStreamEventToIR(data []byte) (*IRStreamEvent, error) {
	var raw openAISSEChunk
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	ev := &IRStreamEvent{
		Raw:  data,
		Type: "content_block_delta",
	}
	if len(raw.Choices) > 0 {
		d := raw.Choices[0].Delta
		if d.Content != "" {
			ev.Delta = &ir.ContentBlock{Type: ir.BlockText, Text: d.Content}
		}
		if len(d.ToolCalls) > 0 {
			for _, tc := range d.ToolCalls {
				if tc.Function.Name != "" {
					ev.Delta = &ir.ContentBlock{
						Type: ir.BlockToolUse,
						ToolUse: &ir.ToolUse{
							ID:    tc.ID,
							Name:  tc.Function.Name,
							Input: parseJSONString(tc.Function.Arguments),
						},
					}
				}
			}
		}
	}
	if raw.Usage != nil {
		ev.Usage = &ir.Usage{
			Input:  raw.Usage.PromptTokens,
			Output: raw.Usage.CompletionTokens,
		}
	}
	return ev, nil
}

// openAI wire shapes
type openAIRequest struct {
	Model         string               `json:"model"`
	Messages      []openAIMessage      `json:"messages"`
	Tools         []openAITool         `json:"tools,omitempty"`
	MaxTokens     int                  `json:"max_tokens,omitempty"`
	Temperature   *float64             `json:"temperature,omitempty"`
	Stream        bool                 `json:"stream"`
	StreamOptions *openAIStreamOptions `json:"stream_options,omitempty"`
}

// openAIStreamOptions carries additional streaming configuration.
// See https://platform.openai.com/docs/api-reference/chat/create#chat-create-stream_options
type openAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    *string          `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIToolFunc `json:"function"`
}

type openAIToolFunc struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type openAISSEChunk struct {
	Choices []struct {
		Delta struct {
			Content   string          `json:"content,omitempty"`
			ToolCalls []openAIDeltaTC `json:"tool_calls,omitempty"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage,omitempty"`
}

type openAIDeltaTC struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function,omitempty"`
}

func openAIRoleToIR(role string) ir.Role {
	switch role {
	case "assistant":
		return ir.RoleAssistant
	case "user":
		return ir.RoleUser
	case "tool":
		return ir.RoleTool
	default:
		return ir.RoleUser
	}
}

func irRoleToOpenAI(r ir.Role) string {
	switch r {
	case ir.RoleAssistant:
		return "assistant"
	case ir.RoleUser:
		return "user"
	case ir.RoleTool:
		return "tool"
	default:
		return "user"
	}
}

func parseJSONString(s string) map[string]any {
	var m map[string]any
	_ = json.Unmarshal([]byte(s), &m)
	return m
}

func ptrStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
