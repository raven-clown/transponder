// Package anthropic reads and writes the Anthropic Messages wire format, both as the server
// answering Anthropic clients and as the client of Anthropic's API.
package anthropic

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/raven-clown/transponder/internal/schema"
)

// DefaultMaxTokens is sent when a translated request sets no max_tokens, which Anthropic requires.
const DefaultMaxTokens = 8192

// RequestError is a problem with the caller's request, reported as a 400 invalid_request_error.
type RequestError struct {
	Message string
}

func (e *RequestError) Error() string { return e.Message }

type wireRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system,omitempty"`
	Messages      []wireMessage   `json:"messages"`
	MaxTokens     *int            `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Tools         []wireTool      `json:"tools,omitempty"`
	ToolChoice    *wireToolChoice `json:"tool_choice,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type wireBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type wireTool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type wireToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// Peek is what the gateway reads from a request before routing it.
type Peek struct {
	Model  string
	Stream bool
}

// PeekRequest reads model and stream by exact key, as Anthropic's API does.
func PeekRequest(body []byte) (Peek, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return Peek{}, &RequestError{Message: "the request body is not a JSON object"}
	}
	var p Peek
	if raw, ok := m["model"]; ok && json.Unmarshal(raw, &p.Model) != nil {
		return Peek{}, &RequestError{Message: "model: must be a string"}
	}
	if raw, ok := m["stream"]; ok && !isNull(raw) && json.Unmarshal(raw, &p.Stream) != nil {
		return Peek{}, &RequestError{Message: "stream: must be true or false"}
	}
	if p.Model == "" {
		return Peek{}, &RequestError{Message: "model: field required"}
	}
	return p, nil
}

func isNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// DecodeRequest reads a Messages request into canonical form, for a target that does not speak
// Anthropic. Thinking blocks are dropped; content no other provider can take is an error.
func DecodeRequest(body []byte) (*schema.ChatRequest, error) {
	var w wireRequest
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, &RequestError{Message: "the request body is not valid JSON: " + err.Error()}
	}
	if w.Model == "" {
		return nil, &RequestError{Message: "model: field required"}
	}
	if len(w.Messages) == 0 {
		return nil, &RequestError{Message: "messages: at least one message is required"}
	}
	req := &schema.ChatRequest{Model: w.Model, MaxTokens: w.MaxTokens, Temperature: w.Temperature, TopP: w.TopP, Stop: w.StopSequences, Stream: w.Stream}
	if !isNull(w.System) {
		text, err := textOf(w.System, "system")
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, schema.Message{Role: schema.RoleSystem, Content: text})
	}
	for _, m := range w.Messages {
		msgs, err := decodeMessage(m)
		if err != nil {
			return nil, err
		}
		req.Messages = append(req.Messages, msgs...)
	}
	for _, t := range w.Tools {
		if t.Type != "" && t.Type != "custom" {
			return nil, &RequestError{Message: "tools: the " + t.Type + " tool runs only on Anthropic, and this route has no Anthropic target"}
		}
		req.Tools = append(req.Tools, schema.Tool{Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
	}
	if tc := w.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto":
			req.ToolChoice = &schema.ToolChoice{Mode: schema.ToolChoiceAuto}
		case "any":
			req.ToolChoice = &schema.ToolChoice{Mode: schema.ToolChoiceRequired}
		case "none":
			req.ToolChoice = &schema.ToolChoice{Mode: schema.ToolChoiceNone}
		case "tool":
			req.ToolChoice = &schema.ToolChoice{Mode: schema.ToolChoiceTool, Name: tc.Name}
		default:
			return nil, &RequestError{Message: "tool_choice: unknown type " + tc.Type}
		}
	}
	return req, nil
}

// blocks reads content given as a string or a list of blocks.
func blocks(raw json.RawMessage) ([]wireBlock, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []wireBlock{{Type: "text", Text: s}}, nil
	}
	var bs []wireBlock
	if err := json.Unmarshal(raw, &bs); err != nil {
		return nil, &RequestError{Message: "content must be a string or a list of content blocks"}
	}
	return bs, nil
}

// textOf joins the text of a string or of text blocks.
func textOf(raw json.RawMessage, where string) (string, error) {
	bs, err := blocks(raw)
	if err != nil {
		return "", err
	}
	parts := make([]string, 0, len(bs))
	for _, b := range bs {
		if b.Type != "text" {
			return "", unsupported(b.Type, where)
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n\n"), nil
}

func unsupported(blockType, where string) *RequestError {
	return &RequestError{Message: where + ": " + blockType + " content can only go to an Anthropic target, and this route has none"}
}

func decodeMessage(m wireMessage) ([]schema.Message, error) {
	bs, err := blocks(m.Content)
	if err != nil {
		return nil, err
	}
	var out []schema.Message
	msg := schema.Message{Role: m.Role}
	var text []string
	for _, b := range bs {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "thinking", "redacted_thinking":
		case "tool_use":
			args := "{}"
			if !isNull(b.Input) {
				args = string(b.Input)
			}
			msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{ID: b.ID, Name: b.Name, Arguments: args})
		case "tool_result":
			content := ""
			if !isNull(b.Content) {
				if content, err = textOf(b.Content, "tool_result"); err != nil {
					return nil, err
				}
			}
			out = append(out, schema.Message{Role: schema.RoleTool, ToolCallID: b.ToolUseID, Content: content})
		default:
			return nil, unsupported(b.Type, "messages")
		}
	}
	msg.Content = strings.Join(text, "")
	if msg.Content != "" || len(msg.ToolCalls) > 0 {
		out = append(out, msg)
	}
	return out, nil
}

// EncodeRequest renders a canonical request as a Messages request. Consecutive tool results
// share one user turn, as Anthropic requires.
func EncodeRequest(req *schema.ChatRequest) ([]byte, error) {
	w := wireRequest{Model: req.Model, MaxTokens: req.MaxTokens, Temperature: req.Temperature, TopP: req.TopP, StopSequences: req.Stop, Stream: req.Stream}
	if w.MaxTokens == nil {
		n := DefaultMaxTokens
		w.MaxTokens = &n
	}
	var system []string
	var turns []struct {
		role   string
		blocks []wireBlock
	}
	add := func(role string, b ...wireBlock) {
		if n := len(turns); n > 0 && turns[n-1].role == role {
			turns[n-1].blocks = append(turns[n-1].blocks, b...)
			return
		}
		turns = append(turns, struct {
			role   string
			blocks []wireBlock
		}{role, b})
	}
	for _, m := range req.Messages {
		switch m.Role {
		case schema.RoleSystem:
			system = append(system, m.Content)
		case schema.RoleTool:
			content, err := json.Marshal(m.Content)
			if err != nil {
				return nil, err
			}
			add("user", wireBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: content})
		case schema.RoleAssistant:
			var bs []wireBlock
			if m.Content != "" {
				bs = append(bs, wireBlock{Type: "text", Text: m.Content})
			}
			for _, c := range m.ToolCalls {
				bs = append(bs, wireBlock{Type: "tool_use", ID: c.ID, Name: c.Name, Input: inputObject(c.Arguments)})
			}
			if len(bs) > 0 {
				add("assistant", bs...)
			}
		default:
			if m.Content != "" {
				add("user", wireBlock{Type: "text", Text: m.Content})
			}
		}
	}
	if len(system) > 0 {
		s, err := json.Marshal(strings.Join(system, "\n\n"))
		if err != nil {
			return nil, err
		}
		w.System = s
	}
	for _, t := range turns {
		content, err := json.Marshal(t.blocks)
		if err != nil {
			return nil, err
		}
		w.Messages = append(w.Messages, wireMessage{Role: t.role, Content: content})
	}
	for _, t := range req.Tools {
		params := t.Parameters
		if isNull(params) {
			params = json.RawMessage(`{"type":"object"}`)
		}
		w.Tools = append(w.Tools, wireTool{Name: t.Name, Description: t.Description, InputSchema: params})
	}
	if tc := req.ToolChoice; tc != nil {
		switch tc.Mode {
		case schema.ToolChoiceRequired:
			w.ToolChoice = &wireToolChoice{Type: "any"}
		case schema.ToolChoiceTool:
			w.ToolChoice = &wireToolChoice{Type: "tool", Name: tc.Name}
		case schema.ToolChoiceNone:
			w.ToolChoice = &wireToolChoice{Type: "none"}
		default:
			w.ToolChoice = &wireToolChoice{Type: "auto"}
		}
	}
	return json.Marshal(w)
}

// inputObject turns tool call arguments into the JSON object Anthropic wants, or {} when they
// are not one.
func inputObject(args string) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(args), &obj) != nil || obj == nil {
		return json.RawMessage(`{}`)
	}
	var b bytes.Buffer
	if json.Compact(&b, []byte(args)) != nil {
		return json.RawMessage(`{}`)
	}
	return b.Bytes()
}

type wireUsage struct {
	InputTokens              int64          `json:"input_tokens"`
	OutputTokens             int64          `json:"output_tokens"`
	CacheCreationInputTokens int64          `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64          `json:"cache_read_input_tokens"`
	CacheCreation            *cacheCreation `json:"cache_creation,omitempty"`
}

type cacheCreation struct {
	Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
}

func (u wireUsage) canonical() schema.Usage {
	out := schema.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadTokens: u.CacheReadInputTokens, CacheWriteTokens: u.CacheCreationInputTokens}
	if u.CacheCreation != nil {
		out.CacheWrite1hTokens = u.CacheCreation.Ephemeral1h
	}
	return out
}

func encodeUsage(u schema.Usage) wireUsage {
	return wireUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheWriteTokens, CacheReadInputTokens: u.CacheReadTokens}
}

type wireResponse struct {
	ID           string      `json:"id"`
	Type         string      `json:"type"`
	Role         string      `json:"role"`
	Model        string      `json:"model"`
	Content      []wireBlock `json:"content"`
	StopReason   string      `json:"stop_reason"`
	StopSequence *string     `json:"stop_sequence"`
	Usage        *wireUsage  `json:"usage"`
}

// EncodeResponse renders a finished canonical call as a Messages response.
func EncodeResponse(resp *schema.ChatResponse) ([]byte, error) {
	w := wireResponse{ID: resp.ID, Type: "message", Role: "assistant", Model: resp.Model, Content: []wireBlock{}, StopReason: stopReason(resp.FinishReason)}
	if w.ID == "" {
		w.ID = NewMessageID()
	}
	if resp.Message.Content != "" {
		w.Content = append(w.Content, wireBlock{Type: "text", Text: resp.Message.Content})
	}
	for _, c := range resp.Message.ToolCalls {
		w.Content = append(w.Content, wireBlock{Type: "tool_use", ID: c.ID, Name: c.Name, Input: inputObject(c.Arguments)})
	}
	u := encodeUsage(resp.Usage)
	w.Usage = &u
	return json.Marshal(w)
}

// DecodeResponse reads a Messages response into canonical form. Thinking blocks are dropped.
func DecodeResponse(body []byte) (*schema.ChatResponse, error) {
	var w wireResponse
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, &RequestError{Message: "anthropic: decoding the response: " + err.Error()}
	}
	resp := &schema.ChatResponse{ID: w.ID, Model: w.Model, FinishReason: finishReason(w.StopReason), Message: schema.Message{Role: schema.RoleAssistant}}
	var text []string
	for _, b := range w.Content {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "tool_use":
			resp.Message.ToolCalls = append(resp.Message.ToolCalls, schema.ToolCall{ID: b.ID, Name: b.Name, Arguments: string(inputObject(string(b.Input)))})
		}
	}
	resp.Message.Content = strings.Join(text, "")
	if w.Usage != nil {
		resp.Usage = w.Usage.canonical()
	}
	return resp, nil
}

// ParseUsage reads the usage of a Messages response.
func ParseUsage(body []byte) (schema.Usage, bool) {
	var w struct {
		Usage *wireUsage `json:"usage"`
	}
	if json.Unmarshal(body, &w) != nil || w.Usage == nil {
		return schema.Usage{}, false
	}
	return w.Usage.canonical(), true
}

func finishReason(stop string) string {
	switch stop {
	case "max_tokens":
		return schema.FinishLength
	case "tool_use":
		return schema.FinishToolCalls
	case "refusal":
		return schema.FinishContentFilter
	default:
		return schema.FinishStop
	}
}

func stopReason(finish string) string {
	switch finish {
	case schema.FinishLength:
		return "max_tokens"
	case schema.FinishToolCalls:
		return "tool_use"
	case schema.FinishContentFilter:
		return "refusal"
	default:
		return "end_turn"
	}
}

// NewMessageID returns an id in the msg_ form Anthropic clients expect.
func NewMessageID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "msg_" + hex.EncodeToString(b)
}
