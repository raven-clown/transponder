package openai

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/schema"
)

func ptr[T any](v T) *T { return &v }

const toolRequest = `{
	"model": "m",
	"messages": [
		{"role": "system", "content": "be brief"},
		{"role": "user", "content": "weather in Paris?"},
		{"role": "assistant", "content": null, "tool_calls": [
			{"id": "call_1", "type": "function", "function": {"name": "weather", "arguments": "{\"city\":\"Paris\"}"}}
		]},
		{"role": "tool", "tool_call_id": "call_1", "content": "sunny"}
	],
	"tools": [{"type": "function", "function": {"name": "weather", "description": "look up weather", "parameters": {"type":"object","properties":{"city":{"type":"string"}}}}}],
	"tool_choice": "required",
	"top_p": 0.5,
	"stop": "END",
	"max_tokens": 100
}`

func toolCanonical() *schema.ChatRequest {
	return &schema.ChatRequest{
		Model: "m",
		Messages: []schema.Message{
			{Role: schema.RoleSystem, Content: "be brief"},
			{Role: schema.RoleUser, Content: "weather in Paris?"},
			{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "call_1", Name: "weather", Arguments: `{"city":"Paris"}`}}},
			{Role: schema.RoleTool, ToolCallID: "call_1", Content: "sunny"},
		},
		Tools:      []schema.Tool{{Name: "weather", Description: "look up weather", Parameters: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}},
		ToolChoice: &schema.ToolChoice{Mode: schema.ToolChoiceRequired},
		TopP:       ptr(0.5),
		Stop:       []string{"END"},
		MaxTokens:  ptr(100),
	}
}

func TestDecodeChatRequestTools(t *testing.T) {
	got, err := ParseChatRequest([]byte(toolRequest))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, toolCanonical()) {
		t.Fatalf("got %+v\nwant %+v", got, toolCanonical())
	}
}

func TestDecodeToolChoiceAndStop(t *testing.T) {
	tests := []struct {
		field string
		want  *schema.ToolChoice
		stop  []string
	}{
		{`"tool_choice": "auto", "stop": ["a", "b"]`, &schema.ToolChoice{Mode: schema.ToolChoiceAuto}, []string{"a", "b"}},
		{`"tool_choice": "none"`, &schema.ToolChoice{Mode: schema.ToolChoiceNone}, nil},
		{`"tool_choice": {"type": "function", "function": {"name": "weather"}}`, &schema.ToolChoice{Mode: schema.ToolChoiceTool, Name: "weather"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			got, err := ParseChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}],` + tt.field + `}`))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.ToolChoice, tt.want) || !reflect.DeepEqual(got.Stop, tt.stop) {
				t.Fatalf("tool choice %+v, stop %v", got.ToolChoice, got.Stop)
			}
		})
	}
	for _, bad := range []string{`"tool_choice": "sometimes"`, `"tool_choice": 3`, `"stop": 3`} {
		_, err := ParseChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}],` + bad + `}`))
		var re *RequestError
		if !errors.As(err, &re) {
			t.Fatalf("%s: err = %v", bad, err)
		}
	}
}

func TestEncodeChatResponseToolCalls(t *testing.T) {
	resp := &schema.ChatResponse{
		ID: "r1", Model: "m", FinishReason: schema.FinishToolCalls,
		Message: schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: "call_1", Name: "weather", Arguments: `{"city":"Paris"}`}}},
		Usage:   schema.Usage{InputTokens: 5, CacheReadTokens: 5, OutputTokens: 2},
	}
	b, err := EncodeChatResponse(resp, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"r1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{\"city\":\"Paris\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":5}}}`
	if string(b) != want {
		t.Fatalf("got\n%s\nwant\n%s", b, want)
	}
}

func TestEncodeChunkTools(t *testing.T) {
	start, err := EncodeChunk("c", "m", time.Unix(1, 0), schema.Event{Type: schema.EventToolStart, ToolIndex: 1, ToolID: "call_1", ToolName: "weather"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(start), `"delta":{"tool_calls":[{"index":1,"id":"call_1","type":"function","function":{"name":"weather","arguments":""}}]}`) {
		t.Fatalf("start frame %s", start)
	}
	delta, err := EncodeChunk("c", "m", time.Unix(1, 0), schema.Event{Type: schema.EventToolDelta, ToolIndex: 1, ToolArgs: `{"ci`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(delta), `"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"ci"}}]}`) {
		t.Fatalf("delta frame %s", delta)
	}
}

func TestStreamUsageMatchesResponseUsage(t *testing.T) {
	u := schema.Usage{InputTokens: 5, CacheReadTokens: 3, CacheWriteTokens: 2, OutputTokens: 1}
	chunk, err := EncodeChunk("c", "m", time.Unix(1, 0), schema.Event{Type: schema.EventUsage, Usage: &u})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(chunk), `"usage":{"prompt_tokens":10,"completion_tokens":1,"total_tokens":11,"prompt_tokens_details":{"cached_tokens":3}}`) {
		t.Fatalf("chunk %s", chunk)
	}
}
