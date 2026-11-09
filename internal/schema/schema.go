// Package schema holds the provider-neutral request and response types every adapter converts to and from.
package schema

import "encoding/json"

// Roles of a message.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Finish reasons.
const (
	FinishStop          = "stop"
	FinishLength        = "length"
	FinishToolCalls     = "tool_calls"
	FinishContentFilter = "content_filter"
)

// Message is one turn of a conversation. An assistant turn may carry tool calls; a tool turn
// carries the result of the call named by ToolCallID.
type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
}

// ToolCall is a request from the model to run a tool. Arguments is JSON text.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// Tool is a function the model may call. Parameters is a JSON Schema.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// Tool choice modes.
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required"
	ToolChoiceTool     = "tool"
)

// ToolChoice says whether and which tool the model must call. Name is set for ToolChoiceTool.
type ToolChoice struct {
	Mode string
	Name string
}

// ChatRequest is a chat call in canonical form. System prompts are messages with RoleSystem.
type ChatRequest struct {
	Model       string
	Messages    []Message
	MaxTokens   *int
	Temperature *float64
	TopP        *float64
	Stop        []string
	Tools       []Tool
	ToolChoice  *ToolChoice
	Stream      bool
}

// ChatResponse is a finished chat call.
type ChatResponse struct {
	ID           string
	Model        string
	Message      Message
	FinishReason string
	Usage        Usage
}

// Usage counts the tokens of one call. InputTokens excludes cache reads and writes.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	// CacheWrite1hTokens is the part of CacheWriteTokens cached for an hour, which Anthropic
	// prices higher than the five-minute default.
	CacheWrite1hTokens int64
	// ReasoningTokens are thinking tokens a provider reports apart from the output. They are
	// billed at the output price. Providers that count them inside the output leave this zero.
	ReasoningTokens int64
}

// EventType names a stream event.
type EventType string

// Stream event types. A stream sends start, then text and tool events, then finish and usage.
const (
	EventStart     EventType = "start"
	EventText      EventType = "text"
	EventToolStart EventType = "tool_start"
	EventToolDelta EventType = "tool_delta"
	EventFinish    EventType = "finish"
	EventUsage     EventType = "usage"
)

// Event is one item of a streamed response. Tool events carry ToolIndex, the position of the
// call in the message; ToolStart also carries ToolID and ToolName, ToolDelta a piece of ToolArgs.
type Event struct {
	Type         EventType
	Text         string
	ToolIndex    int
	ToolID       string
	ToolName     string
	ToolArgs     string
	FinishReason string
	Usage        *Usage
}

// Stream yields events until Recv returns io.EOF.
type Stream interface {
	Recv() (Event, error)
	Close() error
}

// EmbedRequest asks for one vector per input.
type EmbedRequest struct {
	Model string
	Input []string
}

// EmbedResponse holds the vectors in input order.
type EmbedResponse struct {
	Model   string
	Vectors [][]float32
	Usage   Usage
}
