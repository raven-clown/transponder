package openai

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

func TestSSEReader(t *testing.T) {
	in := ": keep-alive\r\n\r\ndata: {\"a\":1}\r\n\r\ndata:{\"b\":2}\n\nevent: x\ndata: {\"c\":3}\n\ndata: [DONE]\n\ndata: {\"after\":1}\n\n"
	r := NewSSEReader(strings.NewReader(in))
	var got []string
	for {
		d, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(d))
	}
	if strings.Join(got, "|") != `{"a":1}|{"b":2}|{"c":3}` {
		t.Fatalf("payloads %q", got)
	}
}

func TestSSEReaderEnds(t *testing.T) {
	r := NewSSEReader(strings.NewReader("data: {\"a\":1}\n\n"))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); !errors.Is(err, ErrNoDone) {
		t.Fatalf("clean end without [DONE] = %v, want ErrNoDone", err)
	}
	r = NewSSEReader(strings.NewReader("data: {\"a\":1}\n\ndata: {\"b\""))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("cut stream = %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestSSEReaderFinalLineWithoutNewline(t *testing.T) {
	r := NewSSEReader(strings.NewReader("data: {\"a\":1}\n\ndata: [DONE]"))
	if _, err := r.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("[DONE] without a newline = %v, want io.EOF", err)
	}
}

func TestChunkUsage(t *testing.T) {
	u, ok, only := ChunkUsage([]byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":3}}`))
	if !ok || !only || u != (schema.Usage{InputTokens: 10, OutputTokens: 3}) {
		t.Fatalf("usage chunk = %+v %v %v", u, ok, only)
	}
	_, ok, only = ChunkUsage([]byte(`{"choices":[{"delta":{"content":"hi"}}]}`))
	if ok || only {
		t.Fatalf("content chunk ok=%v only=%v", ok, only)
	}
	u, ok, only = ChunkUsage([]byte(`{"choices":[{"delta":{}}],"usage":{"prompt_tokens":1}}`))
	if !ok || only || u.InputTokens != 1 {
		t.Fatalf("mixed chunk = %+v %v %v", u, ok, only)
	}
}

func TestEncodeChunk(t *testing.T) {
	created := time.Unix(1700000000, 0)
	head := `{"id":"c1","object":"chat.completion.chunk","created":1700000000,"model":"m",`
	tests := []struct {
		ev   schema.Event
		want string
	}{
		{schema.Event{Type: schema.EventStart}, head + `"choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`},
		{schema.Event{Type: schema.EventText, Text: "hi"}, head + `"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`},
		{schema.Event{Type: schema.EventFinish, FinishReason: "stop"}, head + `"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`},
		{schema.Event{Type: schema.EventUsage, Usage: &schema.Usage{InputTokens: 2, OutputTokens: 3}}, head + `"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`},
	}
	for _, tt := range tests {
		t.Run(string(tt.ev.Type), func(t *testing.T) {
			got, err := EncodeChunk("c1", "m", created, tt.ev)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "data: "+tt.want+"\n\n" {
				t.Fatalf("frame\n%s\nwant\n%s", got, tt.want)
			}
		})
	}
	if got, err := EncodeChunk("c1", "m", created, schema.Event{Type: "other"}); got != nil || err != nil {
		t.Fatalf("unknown event = %q, %v", got, err)
	}
}

func TestEncodeStreamError(t *testing.T) {
	f := string(EncodeStreamError("upstream_error", "cut"))
	if !strings.HasPrefix(f, "data: ") || !strings.HasSuffix(f, "\n\n") {
		t.Fatalf("frame %q", f)
	}
	var v struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(f, "data: "))), &v); err != nil || v.Error.Code != "upstream_error" || v.Error.Message != "cut" {
		t.Fatalf("frame %q: %+v %v", f, v, err)
	}
}

func TestSSEReaderEventTooLarge(t *testing.T) {
	old := sse.MaxEventBytes
	sse.MaxEventBytes = 32
	defer func() { sse.MaxEventBytes = old }()
	_, err := NewSSEReader(strings.NewReader("data: " + strings.Repeat("x", 100))).Next()
	if !errors.Is(err, ErrEventTooLarge) {
		t.Fatalf("err = %v", err)
	}
}
