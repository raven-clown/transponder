package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/dialect/anthropic"
	"github.com/raven-clown/transponder/internal/dialect/gemini"
	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

// FuzzStreamTranslation reads a provider stream in each dialect, as an upstream would send
// it, and writes the events out in the other dialects, as the gateway does for a caller.
// Whatever the upstream sends, every frame written must be one event a client can read,
// whose data is JSON.
func FuzzStreamTranslation(f *testing.F) {
	f.Add([]byte("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"f\",\"arguments\":\"{\\\"a\\\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"))
	f.Add([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"f\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":1}\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	f.Add([]byte("data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"},{\"functionCall\":{\"name\":\"f\",\"args\":{\"a\":1}}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3}}\r\n\r\n"))
	f.Add([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"\\u2028<\"}}\n\n"))
	f.Fuzz(func(t *testing.T, stream []byte) {
		var oa ChunkDecoder
		var an anthropic.StreamDecoder
		var gm gemini.StreamDecoder
		var events []schema.Event
		r := sse.NewReader(bytes.NewReader(stream))
		for range 1000 {
			ev, err := r.Next()
			if err != nil {
				break
			}
			anthropic.OpeningEvent(ev)
			OpeningChunk(ev.Data)
			IsStreamError(ev.Data)
			ChunkUsage(ev.Data)
			for _, decode := range []func() ([]schema.Event, error){
				func() ([]schema.Event, error) { return oa.Events(ev.Data) },
				func() ([]schema.Event, error) { return an.Events(ev) },
				func() ([]schema.Event, error) { return gm.Events(ev.Data) },
			} {
				evs, err := decode()
				if err == nil {
					events = append(events, evs...)
				}
			}
		}
		enc := anthropic.NewStreamEncoder("msg_1", "m")
		var frames [][]byte
		for _, ev := range events {
			frame, err := EncodeChunk("chatcmpl-1", "m", time.Unix(0, 0), ev)
			if err != nil {
				t.Fatalf("encoding %+v: %v", ev, err)
			}
			if frame != nil {
				frames = append(frames, frame)
			}
			frames = append(frames, enc.Frames(ev)...)
		}
		frames = append(frames, enc.End()...)
		for _, frame := range frames {
			checkFrame(t, frame)
		}
	})
}

// checkFrame fails unless frame is exactly one event whose data is JSON.
func checkFrame(t *testing.T, frame []byte) {
	t.Helper()
	r := sse.NewReader(bytes.NewReader(frame))
	ev, err := r.Next()
	if err != nil || !json.Valid(ev.Data) {
		t.Fatalf("frame %q: %v", frame, err)
	}
	if _, err := r.Next(); err == nil || strings.TrimSpace(string(frame)) == "" {
		t.Fatalf("frame %q holds more than one event", frame)
	}
}

// FuzzTranslateRequest decodes a chat request from a caller and encodes it for each other
// dialect, as fallback does. A request that decodes must encode to JSON, or be refused with a
// reason the caller can act on.
func FuzzTranslateRequest(f *testing.F) {
	f.Add([]byte(`{"model":"m","messages":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aGk="}}]},` +
		`{"role":"assistant","tool_calls":[{"id":"functions.f:0","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},{"role":"tool","tool_call_id":"functions.f:0","content":"ok"}],` +
		`"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"tool_choice":"required","stop":["x"],"max_tokens":5}`))
	f.Add([]byte(`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"","type":"function","function":{"name":"","arguments":"not json"}}]}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		req, err := ParseChatRequest(body)
		if err != nil {
			return
		}
		for name, encode := range map[string]func(*schema.ChatRequest) ([]byte, error){
			"openai":    EncodeChatRequest,
			"anthropic": anthropic.EncodeRequest,
			"gemini":    gemini.EncodeRequest,
		} {
			out, err := encode(req)
			if err != nil {
				var ce *schema.ContentError
				if !errors.As(err, &ce) {
					t.Fatalf("%s: %v", name, err)
				}
				continue
			}
			if !json.Valid(out) {
				t.Fatalf("%s: invalid JSON %s", name, out)
			}
		}
	})
}
