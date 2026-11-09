// Package openai reads and writes the OpenAI Chat Completions wire format.
package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/raven-clown/transponder/internal/schema"
)

// RequestError is a problem with the caller's request, reported back as a 400.
type RequestError struct {
	Param   string
	Code    string
	Message string
}

func (e *RequestError) Error() string {
	if e.Param == "" {
		return e.Message
	}
	return fmt.Sprintf("%s: %s", e.Param, e.Message)
}

type wireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []wireToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	Index    *int             `json:"index,omitempty"`
	ID       string           `json:"id,omitempty"`
	Type     string           `json:"type,omitempty"`
	Function wireFunctionCall `json:"function"`
}

type wireFunctionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireRequest struct {
	Model               string             `json:"model"`
	Messages            []wireMessage      `json:"messages"`
	MaxTokens           *int               `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int               `json:"max_completion_tokens,omitempty"`
	Temperature         *float64           `json:"temperature,omitempty"`
	TopP                *float64           `json:"top_p,omitempty"`
	Stop                json.RawMessage    `json:"stop,omitempty"`
	Tools               []wireTool         `json:"tools,omitempty"`
	ToolChoice          json.RawMessage    `json:"tool_choice,omitempty"`
	Stream              bool               `json:"stream,omitempty"`
	StreamOptions       *wireStreamOptions `json:"stream_options,omitempty"`
}

// DecodeChatRequest parses an OpenAI chat completion request.
func DecodeChatRequest(r io.Reader) (*schema.ChatRequest, error) {
	var w wireRequest
	if err := json.NewDecoder(r).Decode(&w); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, err
		}
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) {
			return nil, wrongTypeError(typeErr)
		}
		return nil, &RequestError{Code: "invalid_json", Message: "the request body is not valid JSON: " + err.Error()}
	}
	if w.Model == "" {
		return nil, &RequestError{Param: "model", Code: "missing_field", Message: "model is required"}
	}
	if len(w.Messages) == 0 {
		return nil, &RequestError{Param: "messages", Code: "missing_field", Message: "messages must not be empty"}
	}
	req := &schema.ChatRequest{Model: w.Model, MaxTokens: w.MaxTokens, Temperature: w.Temperature, TopP: w.TopP, Stream: w.Stream}
	if req.MaxTokens == nil {
		req.MaxTokens = w.MaxCompletionTokens
	}
	for _, m := range w.Messages {
		content, err := messageText(m.Content)
		if err != nil {
			return nil, err
		}
		msg := schema.Message{Role: m.Role, Content: content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, schema.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
		}
		req.Messages = append(req.Messages, msg)
	}
	for _, t := range w.Tools {
		req.Tools = append(req.Tools, schema.Tool{Name: t.Function.Name, Description: t.Function.Description, Parameters: t.Function.Parameters})
	}
	var err error
	if req.Stop, err = decodeStop(w.Stop); err != nil {
		return nil, err
	}
	if req.ToolChoice, err = decodeToolChoice(w.ToolChoice); err != nil {
		return nil, err
	}
	return req, nil
}

// messageText reads content given as a string, a list of text parts, or null.
func messageText(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", &RequestError{Param: "messages", Code: "unsupported", Message: "message content must be a string or a list of text parts"}
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Type != "text" {
			return "", &RequestError{Param: "messages", Code: "unsupported", Message: "only text content parts are supported so far"}
		}
		b.WriteString(p.Text)
	}
	return b.String(), nil
}

func decodeStop(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}, nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return nil, &RequestError{Param: "stop", Code: "invalid_json", Message: "stop must be a string or a list of strings"}
	}
	return many, nil
}

func decodeToolChoice(raw json.RawMessage) (*schema.ToolChoice, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	bad := &RequestError{Param: "tool_choice", Code: "invalid_json", Message: `tool_choice must be "auto", "none", "required" or a function`}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case schema.ToolChoiceAuto, schema.ToolChoiceNone, schema.ToolChoiceRequired:
			return &schema.ToolChoice{Mode: mode}, nil
		}
		return nil, bad
	}
	var obj struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil || obj.Function.Name == "" {
		return nil, bad
	}
	return &schema.ToolChoice{Mode: schema.ToolChoiceTool, Name: obj.Function.Name}, nil
}

// wrongTypeError words a JSON type mismatch without naming Go types.
func wrongTypeError(e *json.UnmarshalTypeError) *RequestError {
	if e.Field == "" {
		return &RequestError{Code: "invalid_json", Message: "the request body must be a JSON object"}
	}
	var want string
	switch e.Type.Kind() {
	case reflect.String:
		want = "a string"
	case reflect.Bool:
		want = "true or false"
	case reflect.Int, reflect.Int64:
		want = "an integer"
	case reflect.Float64:
		want = "a number"
	case reflect.Slice:
		want = "a list"
	case reflect.Struct:
		want = "a list of objects"
	default:
		want = "a different type"
	}
	field := dropIndexes(e.Field)
	return &RequestError{Param: field, Code: "invalid_json", Message: field + " must be " + want}
}

// dropIndexes removes numeric path segments, which newer Go versions put in
// UnmarshalTypeError.Field, so the name is the same on every version.
func dropIndexes(field string) string {
	parts := strings.Split(field, ".")
	kept := parts[:0]
	for _, p := range parts {
		if _, err := strconv.Atoi(p); err != nil {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ".")
}

type wireResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []wireChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage,omitempty"`
}

type wireChoice struct {
	Index        int        `json:"index"`
	Message      wireOutMsg `json:"message"`
	FinishReason string     `json:"finish_reason"`
}

type wireOutMsg struct {
	Role      string         `json:"role"`
	Content   *string        `json:"content"`
	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
}

type wireUsage struct {
	PromptTokens            int64                  `json:"prompt_tokens"`
	CompletionTokens        int64                  `json:"completion_tokens"`
	TotalTokens             int64                  `json:"total_tokens"`
	PromptTokensDetails     *wirePromptDetails     `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *wireCompletionDetails `json:"completion_tokens_details,omitempty"`
}

type wireCompletionDetails struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
}

type wirePromptDetails struct {
	CachedTokens int64 `json:"cached_tokens"`
}

func encodeUsage(u schema.Usage) *wireUsage {
	prompt := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
	completion := u.OutputTokens + u.ReasoningTokens
	w := &wireUsage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: prompt + completion}
	if u.CacheReadTokens > 0 {
		w.PromptTokensDetails = &wirePromptDetails{CachedTokens: u.CacheReadTokens}
	}
	if u.ReasoningTokens > 0 {
		w.CompletionTokensDetails = &wireCompletionDetails{ReasoningTokens: u.ReasoningTokens}
	}
	return w
}

func encodeToolCalls(calls []schema.ToolCall) []wireToolCall {
	out := make([]wireToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, wireToolCall{ID: c.ID, Type: "function", Function: wireFunctionCall{Name: c.Name, Arguments: c.Arguments}})
	}
	return out
}

// EncodeChatResponse renders a finished chat call as an OpenAI chat completion.
func EncodeChatResponse(resp *schema.ChatResponse, created time.Time) ([]byte, error) {
	msg := wireOutMsg{Role: resp.Message.Role}
	if msg.Role == "" {
		msg.Role = schema.RoleAssistant
	}
	if len(resp.Message.ToolCalls) > 0 {
		msg.ToolCalls = encodeToolCalls(resp.Message.ToolCalls)
	}
	if resp.Message.Content != "" || len(resp.Message.ToolCalls) == 0 {
		content := resp.Message.Content
		msg.Content = &content
	}
	return json.Marshal(wireResponse{
		ID:      resp.ID,
		Object:  "chat.completion",
		Created: created.Unix(),
		Model:   resp.Model,
		Choices: []wireChoice{{Message: msg, FinishReason: resp.FinishReason}},
		Usage:   encodeUsage(resp.Usage),
	})
}
