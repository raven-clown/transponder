package openai

import (
	"reflect"
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
)

// TestChunkDecoderToolCallsWithoutIndex checks servers that leave out the index of a streamed
// tool call: a new id starts a new call, and a delta without an id continues the last one.
func TestChunkDecoderToolCallsWithoutIndex(t *testing.T) {
	tests := map[string]struct {
		chunks []string
		want   []schema.Event
	}{
		"two whole calls in one chunk": {
			[]string{`{"choices":[{"index":0,"delta":{"tool_calls":[` +
				`{"id":"a","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}},` +
				`{"id":"b","type":"function","function":{"name":"g","arguments":"{\"y\":2}"}}]}}]}`},
			[]schema.Event{
				{Type: schema.EventToolStart, ToolIndex: 0, ToolID: "a", ToolName: "f"},
				{Type: schema.EventToolDelta, ToolIndex: 0, ToolArgs: `{"x":1}`},
				{Type: schema.EventToolStart, ToolIndex: 1, ToolID: "b", ToolName: "g"},
				{Type: schema.EventToolDelta, ToolIndex: 1, ToolArgs: `{"y":2}`},
			},
		},
		"fragments after each call": {
			[]string{
				`{"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"f","arguments":""}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"name":"g","arguments":""}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"arguments":"{}"}}]}}]}`,
			},
			[]schema.Event{
				{Type: schema.EventToolStart, ToolIndex: 0, ToolID: "a", ToolName: "f"},
				{Type: schema.EventToolDelta, ToolIndex: 0, ToolArgs: `{}`},
				{Type: schema.EventToolStart, ToolIndex: 1, ToolID: "b", ToolName: "g"},
				{Type: schema.EventToolDelta, ToolIndex: 1, ToolArgs: `{}`},
			},
		},
		"indexed calls keep their index": {
			[]string{
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"g","arguments":""}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"f","arguments":""}}]}}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]}}]}`,
			},
			[]schema.Event{
				{Type: schema.EventToolStart, ToolIndex: 1, ToolID: "b", ToolName: "g"},
				{Type: schema.EventToolStart, ToolIndex: 0, ToolID: "a", ToolName: "f"},
				{Type: schema.EventToolDelta, ToolIndex: 1, ToolArgs: `{}`},
			},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var d ChunkDecoder
			var got []schema.Event
			for _, c := range tt.chunks {
				evs, err := d.Events([]byte(c))
				if err != nil {
					t.Fatal(err)
				}
				for _, ev := range evs {
					if ev.Type != schema.EventStart {
						got = append(got, ev)
					}
				}
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("events\n got %+v\nwant %+v", got, tt.want)
			}
		})
	}
}
