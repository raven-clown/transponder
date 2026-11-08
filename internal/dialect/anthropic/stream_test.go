package anthropic

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

func TestStreamEncoder(t *testing.T) {
	e := NewStreamEncoder("msg_1", "m")
	var out bytes.Buffer
	for _, ev := range []schema.Event{
		{Type: schema.EventStart},
		{Type: schema.EventText, Text: "Hi"},
		{Type: schema.EventText, Text: "!"},
		{Type: schema.EventToolStart, ToolIndex: 0, ToolID: "call_1", ToolName: "weather"},
		{Type: schema.EventToolDelta, ToolIndex: 0, ToolArgs: `{"city":`},
		{Type: schema.EventToolDelta, ToolIndex: 0, ToolArgs: `"Paris"}`},
		{Type: schema.EventFinish, FinishReason: schema.FinishToolCalls},
		{Type: schema.EventUsage, Usage: &schema.Usage{InputTokens: 7, OutputTokens: 3}},
	} {
		for _, f := range e.Frames(ev) {
			out.Write(f)
		}
	}
	for _, f := range e.End() {
		out.Write(f)
	}
	want := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":0,"output_tokens":0}}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"!"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"weather","input":{}}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":1}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":7,"output_tokens":3}}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}
	got := strings.Split(strings.TrimSpace(out.String()), "\n\n")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("frames\n%s", out.String())
	}
}

func TestStreamEncoderStartsLazily(t *testing.T) {
	e := NewStreamEncoder("msg_1", "m")
	frames := e.Frames(schema.Event{Type: schema.EventText, Text: "x"})
	if len(frames) != 3 || !bytes.HasPrefix(frames[0], []byte("event: message_start")) {
		t.Fatalf("frames %q", frames)
	}
	if f := ErrorFrame("api_error", "cut"); !bytes.Equal(f, []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"cut\"}}\n\n")) {
		t.Fatalf("error frame %q", f)
	}
}

const nativeStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":12,\"cache_read_input_tokens\":30,\"cache_creation_input_tokens\":5,\"output_tokens\":1}}}\n\n" +
	"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"s\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"weather\",\"input\":{}}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":9}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func TestStreamDecoder(t *testing.T) {
	r := sse.NewReader(strings.NewReader(nativeStream))
	var d StreamDecoder
	var got []schema.Event
	for {
		ev, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		evs, err := d.Events(ev)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, evs...)
	}
	usage := schema.Usage{InputTokens: 12, CacheReadTokens: 30, CacheWriteTokens: 5, OutputTokens: 9}
	want := []schema.Event{
		{Type: schema.EventStart},
		{Type: schema.EventText, Text: "Hi"},
		{Type: schema.EventToolStart, ToolIndex: 0, ToolID: "toolu_1", ToolName: "weather"},
		{Type: schema.EventToolDelta, ToolIndex: 0, ToolArgs: "{}"},
		{Type: schema.EventFinish, FinishReason: schema.FinishToolCalls},
		{Type: schema.EventUsage, Usage: &usage},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("events\n got %+v\nwant %+v", got, want)
	}
	if !d.Done() || d.Usage() != usage {
		t.Fatalf("done %v usage %+v", d.Done(), d.Usage())
	}
}

func TestStreamDecoderError(t *testing.T) {
	var d StreamDecoder
	_, err := d.Events(sse.Event{Name: "error", Data: []byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`)})
	var se *StreamError
	if !errors.As(err, &se) || se.Type != "overloaded_error" || se.Message != "busy" {
		t.Fatalf("err = %v", err)
	}
	if _, err := d.Events(sse.Event{Data: []byte(`nope`)}); err == nil {
		t.Fatal("bad event accepted")
	}
	if d.Done() {
		t.Fatal("done without message_stop")
	}
}
