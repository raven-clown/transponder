package anthropic

import (
	"testing"

	"github.com/raven-clown/transponder/internal/sse"
)

func TestOpeningEvent(t *testing.T) {
	tests := []struct {
		data string
		want bool
	}{
		{`{"type":"message_start","message":{"usage":{"input_tokens":3}}}`, true},
		{`{"type":"ping"}`, true},
		{`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, true},
		{`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hi"}}`, false},
		{`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"f"}}`, false},
		{`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`, false},
		{`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`, false},
		{`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`, false},
		{`{"type":"message_stop"}`, false},
		{`{"type":"error","error":{"type":"overloaded_error"}}`, false},
		{`{"type":`, false},
	}
	for _, tt := range tests {
		if got := OpeningEvent(sse.Event{Data: []byte(tt.data)}); got != tt.want {
			t.Errorf("OpeningEvent(%s) = %v, want %v", tt.data, got, tt.want)
		}
	}
}
