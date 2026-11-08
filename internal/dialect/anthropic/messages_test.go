package anthropic

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
)

func ptr[T any](v T) *T { return &v }

const claudeCodeRequest = `{
	"model": "claude-sonnet-5-5",
	"max_tokens": 1024,
	"system": [{"type": "text", "text": "You are terse.", "cache_control": {"type": "ephemeral"}}, {"type": "text", "text": "Use tools."}],
	"thinking": {"type": "adaptive"},
	"messages": [
		{"role": "user", "content": "weather in Paris?"},
		{"role": "assistant", "content": [
			{"type": "thinking", "thinking": "", "signature": "sig"},
			{"type": "text", "text": "Checking."},
			{"type": "tool_use", "id": "toolu_1", "name": "weather", "input": {"city": "Paris"}}
		]},
		{"role": "user", "content": [
			{"type": "tool_result", "tool_use_id": "toolu_1", "content": [{"type": "text", "text": "sunny"}]},
			{"type": "text", "text": "and tomorrow?"}
		]}
	],
	"tools": [{"name": "weather", "description": "look up weather", "input_schema": {"type": "object"}}],
	"tool_choice": {"type": "any"},
	"stop_sequences": ["END"],
	"stream": true
}`

func claudeCodeCanonical() *schema.ChatRequest {
	return &schema.ChatRequest{
		Model: "claude-sonnet-5-5",
		Messages: []schema.Message{
			{Role: schema.RoleSystem, Content: "You are terse.\n\nUse tools."},
			{Role: schema.RoleUser, Content: "weather in Paris?"},
			{Role: schema.RoleAssistant, Content: "Checking.", ToolCalls: []schema.ToolCall{{ID: "toolu_1", Name: "weather", Arguments: `{"city": "Paris"}`}}},
			{Role: schema.RoleTool, ToolCallID: "toolu_1", Content: "sunny"},
			{Role: schema.RoleUser, Content: "and tomorrow?"},
		},
		MaxTokens:  ptr(1024),
		Stop:       []string{"END"},
		Tools:      []schema.Tool{{Name: "weather", Description: "look up weather", Parameters: json.RawMessage(`{"type": "object"}`)}},
		ToolChoice: &schema.ToolChoice{Mode: schema.ToolChoiceRequired},
		Stream:     true,
	}
}

func TestDecodeRequest(t *testing.T) {
	got, err := DecodeRequest([]byte(claudeCodeRequest))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, claudeCodeCanonical()) {
		t.Fatalf("got  %+v\nwant %+v", got, claudeCodeCanonical())
	}
}

func TestDecodeRequestRejects(t *testing.T) {
	for name, body := range map[string]string{
		"not json":      `{`,
		"no model":      `{"messages":[{"role":"user","content":"x"}]}`,
		"no messages":   `{"model":"m"}`,
		"image":         `{"model":"m","messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`,
		"server tool":   `{"model":"m","messages":[{"role":"user","content":"x"}],"tools":[{"type":"web_search_20260209","name":"web_search"}]}`,
		"bad choice":    `{"model":"m","messages":[{"role":"user","content":"x"}],"tool_choice":{"type":"maybe"}}`,
		"image in tool": `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image"}]}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeRequest([]byte(body))
			var re *RequestError
			if !errors.As(err, &re) || re.Message == "" {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestEncodeRequestRoundTrip(t *testing.T) {
	want := claudeCodeCanonical()
	b, err := EncodeRequest(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeRequest(b)
	if err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	want.Messages[2].ToolCalls[0].Arguments = `{"city":"Paris"}`
	got.Tools[0].Parameters = json.RawMessage(`{"type": "object"}`)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip\n got %+v\nwant %+v\nwire %s", got, want, b)
	}
}

func TestEncodeRequestShapes(t *testing.T) {
	req := &schema.ChatRequest{
		Model: "m",
		Messages: []schema.Message{
			{Role: schema.RoleUser, Content: "x"},
			{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "a", Name: "f", Arguments: "not json"}, {ID: "b", Name: "g", Arguments: ""}}},
			{Role: schema.RoleTool, ToolCallID: "a", Content: "1"},
			{Role: schema.RoleTool, ToolCallID: "b", Content: "2"},
		},
		ToolChoice: &schema.ToolChoice{Mode: schema.ToolChoiceTool, Name: "f"},
	}
	b, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		MaxTokens  int             `json:"max_tokens"`
		ToolChoice json.RawMessage `json:"tool_choice"`
		Messages   []struct {
			Role    string `json:"role"`
			Content []struct {
				Type      string          `json:"type"`
				Input     json.RawMessage `json:"input"`
				ToolUseID string          `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		t.Fatal(err)
	}
	if w.MaxTokens != DefaultMaxTokens || string(w.ToolChoice) != `{"type":"tool","name":"f"}` {
		t.Fatalf("wire %s", b)
	}
	if len(w.Messages) != 3 || len(w.Messages[2].Content) != 2 || w.Messages[2].Content[1].ToolUseID != "b" {
		t.Fatalf("tool results not grouped into one user turn: %s", b)
	}
	if string(w.Messages[1].Content[0].Input) != `{}` || string(w.Messages[1].Content[1].Input) != `{}` {
		t.Fatalf("bad arguments not replaced by an empty object: %s", b)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	resp := &schema.ChatResponse{
		ID: "msg_1", Model: "m", FinishReason: schema.FinishToolCalls,
		Message: schema.Message{Role: schema.RoleAssistant, Content: "Checking.", ToolCalls: []schema.ToolCall{{ID: "toolu_1", Name: "weather", Arguments: `{"city":"Paris"}`}}},
		Usage:   schema.Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3, CacheWriteTokens: 2},
	}
	b, err := EncodeResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"Checking."},{"type":"tool_use","id":"toolu_1","name":"weather","input":{"city":"Paris"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":2,"cache_read_input_tokens":3}}`
	if string(b) != want {
		t.Fatalf("got\n%s\nwant\n%s", b, want)
	}
	got, err := DecodeResponse(b)
	if err != nil || !reflect.DeepEqual(got, resp) {
		t.Fatalf("decoded %+v, %v", got, err)
	}
	u, ok := ParseUsage(b)
	if !ok || u != resp.Usage {
		t.Fatalf("usage %+v %v", u, ok)
	}
	if _, ok := ParseUsage([]byte(`{}`)); ok {
		t.Fatal("usage found in an empty body")
	}
}

func TestStopReasons(t *testing.T) {
	for wire, canon := range map[string]string{"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length", "tool_use": "tool_calls", "refusal": "content_filter", "pause_turn": "stop"} {
		if got := finishReason(wire); got != canon {
			t.Errorf("finishReason(%s) = %s", wire, got)
		}
	}
	for canon, wire := range map[string]string{"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use", "content_filter": "refusal", "": "end_turn"} {
		if got := stopReason(canon); got != wire {
			t.Errorf("stopReason(%s) = %s", canon, got)
		}
	}
}

func TestPeekRequest(t *testing.T) {
	p, err := PeekRequest([]byte(`{"model":"m","stream":true,"Stream":false}`))
	if err != nil || p.Model != "m" || !p.Stream {
		t.Fatalf("peek %+v, %v", p, err)
	}
	for _, bad := range []string{`{`, `{"stream":true}`, `{"model":3}`} {
		if _, err := PeekRequest([]byte(bad)); err == nil {
			t.Fatalf("%s accepted", bad)
		}
	}
}

func TestWriteError(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, 529, ErrorType(529), "busy")
	if rec.Code != 529 || rec.Body.String() != `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}` || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for status, want := range map[int]string{400: "invalid_request_error", 401: "authentication_error", 403: "permission_error", 404: "not_found_error", 413: "request_too_large", 429: "rate_limit_error", 500: "api_error", 502: "api_error", 504: "api_error"} {
		if got := ErrorType(status); got != want {
			t.Errorf("ErrorType(%d) = %s", status, got)
		}
	}
}
