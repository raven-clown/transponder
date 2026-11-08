package openai

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
)

func TestEncodeChatRequestRoundTrip(t *testing.T) {
	want := toolCanonical()
	b, err := EncodeChatRequest(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseChatRequest(b)
	if err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip\n got %+v\nwant %+v\nwire %s", got, want, b)
	}
	var wire map[string]json.RawMessage
	_ = json.Unmarshal(b, &wire)
	if string(wire["tool_choice"]) != `"required"` {
		t.Fatalf("tool_choice %s", wire["tool_choice"])
	}

	named := &schema.ChatRequest{Model: "m", Messages: []schema.Message{{Role: "user", Content: "x"}}, Stream: true,
		ToolChoice: &schema.ToolChoice{Mode: schema.ToolChoiceTool, Name: "f"}}
	b, err = EncodeChatRequest(named)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(b, &wire)
	if string(wire["tool_choice"]) != `{"type":"function","function":{"name":"f"}}` || string(wire["stream_options"]) != `{"include_usage":true}` {
		t.Fatalf("wire %s", b)
	}
}

func TestDecodeChatResponse(t *testing.T) {
	body := `{"id":"r","model":"gpt-x","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"weather","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"prompt_tokens_details":{"cached_tokens":4}}}`
	got, err := DecodeChatResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := &schema.ChatResponse{ID: "r", Model: "gpt-x", FinishReason: schema.FinishToolCalls,
		Message: schema.Message{Role: "assistant", ToolCalls: []schema.ToolCall{{ID: "call_1", Name: "weather", Arguments: "{}"}}},
		Usage:   schema.Usage{InputTokens: 6, CacheReadTokens: 4, OutputTokens: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
	if _, err := DecodeChatResponse([]byte(`{"choices":[]}`)); err == nil {
		t.Fatal("a response without choices should be an error")
	}
}

func TestChunkDecoder(t *testing.T) {
	chunks := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"Hi"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"weather","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"c"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":2}}`,
	}
	var d ChunkDecoder
	var got []schema.Event
	for _, c := range chunks {
		evs, err := d.Events([]byte(c))
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, evs...)
	}
	want := []schema.Event{
		{Type: schema.EventStart},
		{Type: schema.EventText, Text: "Hi"},
		{Type: schema.EventToolStart, ToolIndex: 0, ToolID: "call_1", ToolName: "weather"},
		{Type: schema.EventToolDelta, ToolIndex: 0, ToolArgs: `{"c`},
		{Type: schema.EventFinish, FinishReason: schema.FinishToolCalls},
		{Type: schema.EventUsage, Usage: &schema.Usage{InputTokens: 7, OutputTokens: 2}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events\n got %+v\nwant %+v", got, want)
	}
	if _, err := d.Events([]byte(`nope`)); err == nil {
		t.Fatal("bad chunk should be an error")
	}
}

func TestEmbedWire(t *testing.T) {
	b, err := EncodeEmbedRequest(&schema.EmbedRequest{Model: "e", Input: []string{"a", "b"}})
	if err != nil || string(b) != `{"model":"e","input":["a","b"]}` {
		t.Fatalf("%s %v", b, err)
	}
	resp, err := DecodeEmbedResponse([]byte(`{"model":"e","data":[{"index":1,"embedding":[2]},{"index":0,"embedding":[1]}],"usage":{"prompt_tokens":3}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resp.Vectors, [][]float32{{1}, {2}}) || resp.Usage.InputTokens != 3 || resp.Model != "e" {
		t.Fatalf("resp %+v", resp)
	}
}

func TestChunkDecoderRepeatedToolStart(t *testing.T) {
	var d ChunkDecoder
	var starts int
	for _, c := range []string{
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{\"a\""}}]}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":":1}"}}]}}]}`,
	} {
		evs, err := d.Events([]byte(c))
		if err != nil {
			t.Fatal(err)
		}
		for _, ev := range evs {
			if ev.Type == schema.EventToolStart {
				starts++
			}
		}
	}
	if starts != 1 {
		t.Fatalf("%d tool starts for one call", starts)
	}
}
