package anthropic

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

var anthropicID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// TestToolIDsAnthropicAccepts checks tool call ids made by other providers, such as Kimi's
// functions.get_weather:0: Anthropic takes only letters, digits, _ and -, so they are
// rewritten the same way wherever they appear, and a call still meets its result.
func TestToolIDsAnthropicAccepts(t *testing.T) {
	odd := "functions.get_weather:0"
	req := &schema.ChatRequest{Model: "m", Messages: []schema.Message{
		{Role: schema.RoleUser, Content: schema.Text("weather?")},
		{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: odd, Name: "get_weather", Arguments: `{}`}, {ID: "call_ok-1", Name: "f", Arguments: `{}`}}},
		{Role: schema.RoleTool, ToolCallID: odd, Content: schema.Text("sunny")},
		{Role: schema.RoleTool, ToolCallID: "call_ok-1", Content: schema.Text("x")},
	}}
	body, err := EncodeRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Messages []struct {
			Content []struct {
				Type      string `json:"type"`
				ID        string `json:"id"`
				ToolUseID string `json:"tool_use_id"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		t.Fatal(err)
	}
	var uses, results []string
	for _, m := range w.Messages {
		for _, b := range m.Content {
			switch b.Type {
			case "tool_use":
				uses = append(uses, b.ID)
			case "tool_result":
				results = append(results, b.ToolUseID)
			}
		}
	}
	if len(uses) != 2 || len(results) != 2 || uses[0] != results[0] || uses[1] != "call_ok-1" || results[1] != "call_ok-1" {
		t.Fatalf("tool_use ids %q, tool_result ids %q", uses, results)
	}
	if !anthropicID.MatchString(uses[0]) {
		t.Fatalf("id %q is not one Anthropic takes", uses[0])
	}

	// An answer translated for an Anthropic caller carries ids it can send back to Anthropic.
	out, err := EncodeResponse(&schema.ChatResponse{Message: schema.Message{Role: schema.RoleAssistant, ToolCalls: []schema.ToolCall{{ID: odd, Name: "f", Arguments: `{}`}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"id":"`+uses[0]+`"`) {
		t.Fatalf("response %s, want id %s", out, uses[0])
	}
	e := NewStreamEncoder("msg", "m")
	var frames bytes.Buffer
	for _, ev := range []schema.Event{{Type: schema.EventToolStart, ToolID: odd, ToolName: "f"}, {Type: schema.EventFinish, FinishReason: schema.FinishToolCalls}} {
		for _, f := range e.Frames(ev) {
			frames.Write(f)
		}
	}
	if !strings.Contains(frames.String(), `"id":"`+uses[0]+`"`) {
		t.Fatalf("stream %s, want id %s", frames.String(), uses[0])
	}
}

// TestStreamDecoderToolInput checks the arguments an OpenAI caller gets for an Anthropic tool
// call. A tool that takes no input may get no input_json_delta, and a server that sends the
// whole input in content_block_start may send no delta at all; either way the arguments must
// parse.
func TestStreamDecoderToolInput(t *testing.T) {
	tests := map[string]struct {
		start, deltas, want string
	}{
		"no input":            {`{}`, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":""}}`, `{}`},
		"no delta at all":     {`{}`, ``, `{}`},
		"input in the start":  {`{"city":"Paris"}`, ``, `{"city":"Paris"}`},
		"input in the deltas": {`{}`, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`, `{"a":1}`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			events := []string{
				`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"now","input":` + tt.start + `}}`,
				tt.deltas,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
				`{"type":"message_stop"}`,
			}
			var d StreamDecoder
			var args strings.Builder
			for _, data := range events {
				if data == "" {
					continue
				}
				evs, err := d.Events(sse.Event{Data: []byte(data)})
				if err != nil {
					t.Fatal(err)
				}
				for _, e := range evs {
					args.WriteString(e.ToolArgs)
				}
			}
			if args.String() != tt.want {
				t.Fatalf("arguments %q, want %q", args.String(), tt.want)
			}
		})
	}
}
