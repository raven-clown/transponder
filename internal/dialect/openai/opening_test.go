package openai

import (
	"testing"

	"github.com/raven-clown/transponder/internal/sse"
)

func TestOpeningChunk(t *testing.T) {
	tests := map[string]struct {
		data string
		want bool
	}{
		"role chunk":            {`{"choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`, true},
		"role with null fields": {`{"choices":[{"index":0,"delta":{"role":"assistant","content":null,"refusal":null,"tool_calls":[]},"finish_reason":null}]}`, true},
		"azure filter results":  {`{"choices":[],"prompt_filter_results":[{"prompt_index":0}]}`, true},
		"text":                  {`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`, false},
		"reasoning":             {`{"choices":[{"index":0,"delta":{"reasoning_content":"hmm"}}]}`, false},
		"tool call":             {`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"f"}}]}}]}`, false},
		"finish":                {`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, false},
		"usage":                 {`{"choices":[],"usage":{"prompt_tokens":1}}`, false},
		"null usage":            {`{"choices":[{"index":0,"delta":{"role":"assistant"}}],"usage":null}`, true},
		"error":                 {`{"error":{"message":"x"}}`, false},
		"not json":              {`hello`, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := OpeningChunk([]byte(tt.data)); got != tt.want {
				t.Fatalf("OpeningChunk = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpeningResponseEvent(t *testing.T) {
	tests := []struct {
		data string
		want bool
	}{
		{`{"type":"response.queued"}`, true},
		{`{"type":"response.created"}`, true},
		{`{"type":"response.in_progress"}`, true},
		{`{"type":"response.output_item.added"}`, false},
		{`{"type":"response.output_text.delta"}`, false},
		{`{"type":"error","code":"server_error"}`, false},
		{`{"type":"response.completed","response":{}}`, false},
		{`{"type":`, false},
	}
	for _, tt := range tests {
		if got := OpeningResponseEvent(sse.Event{Data: []byte(tt.data)}); got != tt.want {
			t.Errorf("OpeningResponseEvent(%s) = %v, want %v", tt.data, got, tt.want)
		}
	}
}
