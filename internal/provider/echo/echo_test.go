package echo

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/schema"
)

func chat(t *testing.T, msgs ...schema.Message) *schema.ChatResponse {
	t.Helper()
	resp, err := New("echo").Chat(context.Background(), &schema.ChatRequest{Model: "m", Messages: msgs})
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestEchoReply(t *testing.T) {
	resp := chat(t,
		schema.Message{Role: "system", Content: "be brief"},
		schema.Message{Role: "user", Content: "hi"},
		schema.Message{Role: "assistant", Content: "yo"},
		schema.Message{Role: "user", Content: "ping"},
	)
	if resp.Message.Content != "echo: ping" || resp.Message.Role != "assistant" || resp.FinishReason != "stop" || resp.Model != "m" {
		t.Fatalf("resp = %+v", resp)
	}
	if resp.ID == "" {
		t.Fatal("empty ID")
	}
}

func TestEchoUsage(t *testing.T) {
	u := chat(t, schema.Message{Role: "user", Content: "abcdefgh"}).Usage
	if u.InputTokens != 2 || u.OutputTokens != 4 {
		t.Fatalf("usage = %+v, want input 2 output 4", u)
	}
}

func TestEchoUsageCountsRunes(t *testing.T) {
	if u := chat(t, schema.Message{Role: "user", Content: "สวัสดี"}).Usage; u.InputTokens != 2 {
		t.Fatalf("input tokens = %d, want 2", u.InputTokens)
	}
}

func TestEchoNoUserMessage(t *testing.T) {
	if got := chat(t, schema.Message{Role: "system", Content: "x"}).Message.Content; got != "echo: " {
		t.Fatalf("content = %q", got)
	}
}

func TestEchoNameAndDialect(t *testing.T) {
	e := New("echo")
	if e.Name() != "echo" || e.Dialect() != "none" {
		t.Fatalf("name %q dialect %q", e.Name(), e.Dialect())
	}
}

func TestEchoStream(t *testing.T) {
	req := &schema.ChatRequest{Model: "m", Messages: []schema.Message{{Role: "user", Content: "hello there  friend"}}}
	want, _ := New("echo").Chat(context.Background(), req)
	st, err := New("echo").ChatStream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var types []schema.EventType
	var text strings.Builder
	var usage *schema.Usage
	for {
		ev, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, ev.Type)
		text.WriteString(ev.Text)
		if ev.Usage != nil {
			usage = ev.Usage
		}
		if ev.Type == schema.EventFinish && ev.FinishReason != "stop" {
			t.Fatalf("finish reason %q", ev.FinishReason)
		}
	}
	if text.String() != want.Message.Content {
		t.Fatalf("streamed %q, want %q", text.String(), want.Message.Content)
	}
	if usage == nil || *usage != want.Usage {
		t.Fatalf("usage %+v, want %+v", usage, want.Usage)
	}
	if types[0] != schema.EventStart || types[len(types)-2] != schema.EventFinish || types[len(types)-1] != schema.EventUsage {
		t.Fatalf("event order %v", types)
	}
	for _, ty := range types[1 : len(types)-2] {
		if ty != schema.EventText {
			t.Fatalf("event order %v", types)
		}
	}
	if len(types) < 5 {
		t.Fatalf("reply was not split into words: %v", types)
	}
}

func TestEchoEmbed(t *testing.T) {
	e := New("echo")
	resp, err := e.Embed(context.Background(), &schema.EmbedRequest{Model: "e", Input: []string{"a", "b", "a"}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Model != "e" || len(resp.Vectors) != 3 || len(resp.Vectors[0]) != 8 {
		t.Fatalf("response %+v", resp)
	}
	if !reflect.DeepEqual(resp.Vectors[0], resp.Vectors[2]) || reflect.DeepEqual(resp.Vectors[0], resp.Vectors[1]) {
		t.Fatalf("vectors %v", resp.Vectors)
	}
	for _, v := range resp.Vectors[0] {
		if v < 0 || v > 1 {
			t.Fatalf("value %v out of range", v)
		}
	}
	if resp.Usage.InputTokens != 3 {
		t.Fatalf("usage %+v", resp.Usage)
	}
}
