// Package ir defines the gateway's internal representation types (§10.0).
// These are semantically aligned with the Anthropic Messages API but are
// standalone types — no dependency on github.com/anthropics/anthropic-sdk-go.
package ir

// Message represents a single message in a conversation.
type Message struct {
	Role    Role           `json:"role"`
	Content []ContentBlock `json:"content"`
}

// Role is the speaker of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ContentBlock is a single block within a message.
type ContentBlock struct {
	Type         BlockType         `json:"type"`
	Text         string            `json:"text,omitempty"`
	ToolUse      *ToolUse          `json:"tool_use,omitempty"`
	ToolResult   *ToolResult       `json:"tool_result,omitempty"`
	Thinking     *ThinkingBlock    `json:"thinking,omitempty"`
	CacheControl *CacheControlSpec `json:"cache_control,omitempty"`
}

// BlockType enumerates content block variants.
type BlockType string

const (
	BlockText       BlockType = "text"
	BlockToolUse    BlockType = "tool_use"
	BlockToolResult BlockType = "tool_result"
	BlockThinking   BlockType = "thinking"
)

// Tool describes a function the model may call.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// ToolChoice controls tool selection behaviour.
type ToolChoice struct {
	Type ToolChoiceType `json:"type"`
	Name string         `json:"name,omitempty"`
}

// ToolChoiceType enumerates tool choice modes.
type ToolChoiceType string

const (
	ToolChoiceAuto    ToolChoiceType = "auto"
	ToolChoiceAny     ToolChoiceType = "any"
	ToolChoiceSpecific ToolChoiceType = "specific"
)

// ToolUse represents a tool invocation by the model.
type ToolUse struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

// ToolResult carries the result of a tool invocation.
type ToolResult struct {
	ToolUseID string         `json:"tool_use_id"`
	Content   []ContentBlock `json:"content"`
	IsError   bool           `json:"is_error,omitempty"`
}

// ThinkingBlock holds the model's internal reasoning.
type ThinkingBlock struct {
	Text string `json:"text"`
}

// CacheControlSpec marks a content block for ephemeral caching.
type CacheControlSpec struct {
	Type string `json:"type"` // "ephemeral"
	TTL  string `json:"ttl,omitempty"`
}

// ThinkingSpec configures extended thinking.
type ThinkingSpec struct {
	Enabled      bool `json:"enabled"`
	BudgetTokens int  `json:"budget_tokens"`
}

// StopReason indicates why the model stopped generating.
type StopReason string

const (
	StopEndTurn   StopReason = "end_turn"
	StopMaxTokens StopReason = "max_tokens"
	StopToolUse   StopReason = "tool_use"
	StopStopSeq   StopReason = "stop_sequence"
)

// Usage carries token usage information.
type Usage struct {
	Input       int `json:"input"`
	Output      int `json:"output"`
	CacheRead   int `json:"cache_read"`
	CacheCreate int `json:"cache_create"`
}

// StreamEventType enumerates SSE event types.
type StreamEventType string

const (
	StreamMessageStart      StreamEventType = "message_start"
	StreamContentBlockStart StreamEventType = "content_block_start"
	StreamContentBlockDelta StreamEventType = "content_block_delta"
	StreamContentBlockStop  StreamEventType = "content_block_stop"
	StreamMessageDelta      StreamEventType = "message_delta"
	StreamMessageStop       StreamEventType = "message_stop"
	StreamAICGUsage         StreamEventType = "aicg_usage"
)
