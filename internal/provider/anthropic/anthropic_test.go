package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
)

func TestDefaults(t *testing.T) {
	a := New("claude", "", "k", nil)
	if a.BaseURL() != "https://api.anthropic.com/v1" || a.Name() != "claude" || a.Dialect() != "anthropic" {
		t.Fatalf("adapter %q %q %q", a.Name(), a.BaseURL(), a.Dialect())
	}
}

func TestNativeHeaders(t *testing.T) {
	tests := []struct {
		name        string
		in          http.Header
		wantVersion string
		wantBeta    string
	}{
		{"default version", http.Header{}, "2023-06-01", ""},
		{"caller version and beta", http.Header{"Anthropic-Version": {"2026-10-21"}, "Anthropic-Beta": {"a,b"}, "X-Api-Key": {"tp_caller"}, "X-Request-Id": {"r1"}}, "2026-10-21", "a,b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got *http.Request
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r }))
			defer srv.Close()
			resp, err := New("claude", srv.URL, "sk-ant", srv.Client()).Native(context.Background(),
				&provider.NativeCall{Path: "/messages/count_tokens", Body: []byte(`{}`), Header: tt.in})
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if got.URL.Path != "/messages/count_tokens" || got.Header.Get("X-Api-Key") != "sk-ant" ||
				got.Header.Get("Anthropic-Version") != tt.wantVersion || got.Header.Get("Anthropic-Beta") != tt.wantBeta ||
				got.Header.Get("Authorization") != "" {
				t.Fatalf("upstream request %s %v", got.URL.Path, got.Header)
			}
			if tt.in.Get("X-Request-ID") != "" && got.Header.Get("X-Request-ID") != "r1" {
				t.Fatalf("request id not forwarded: %v", got.Header)
			}
		})
	}
}

func TestNativeOverloaded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(529) }))
	defer srv.Close()
	resp, err := New("claude", srv.URL, "k", nil).Native(context.Background(), &provider.NativeCall{Path: "/messages", Header: http.Header{}})
	if resp != nil {
		_ = resp.Body.Close()
	}
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Status != 529 || !pe.Retryable {
		t.Fatalf("err = %v", err)
	}
}

func TestChat(t *testing.T) {
	var got map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"thinking","thinking":""},{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":1,"cache_read_input_tokens":2}}`)
	}))
	defer srv.Close()
	temp := 0.7
	resp, err := New("claude", srv.URL, "k", nil).Chat(context.Background(), &schema.ChatRequest{
		Model: "claude-x", Temperature: &temp, Messages: []schema.Message{{Role: "system", Content: "terse"}, {Role: "user", Content: "x"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "hi" || resp.FinishReason != "stop" || resp.Usage != (schema.Usage{InputTokens: 4, OutputTokens: 1, CacheReadTokens: 2}) {
		t.Fatalf("resp %+v", resp)
	}
	if _, ok := got["temperature"]; ok || string(got["max_tokens"]) != "8192" || string(got["system"]) != `"terse"` {
		t.Fatalf("upstream body %v", got)
	}
}

const streamBody = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4}}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n"

func collect(t *testing.T, st schema.Stream) ([]schema.EventType, error) {
	t.Helper()
	defer func() { _ = st.Close() }()
	var types []schema.EventType
	for {
		ev, err := st.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return types, nil
			}
			return types, err
		}
		types = append(types, ev.Type)
	}
}

func TestChatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, streamBody+"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	st, err := New("claude", srv.URL, "k", nil).ChatStream(context.Background(), &schema.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	types, err := collect(t, st)
	want := []schema.EventType{schema.EventStart, schema.EventText, schema.EventFinish, schema.EventUsage}
	if err != nil || !reflect.DeepEqual(types, want) {
		t.Fatalf("events %v, %v", types, err)
	}
}

func TestChatStreamCutAndError(t *testing.T) {
	for name, body := range map[string]string{
		"no message_stop": streamBody,
		"error event":     streamBody + "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
			defer srv.Close()
			st, err := New("claude", srv.URL, "k", nil).ChatStream(context.Background(), &schema.ChatRequest{Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := collect(t, st); err == nil {
				t.Fatal("stream ended cleanly")
			}
		})
	}
}

func TestEmbedUnsupported(t *testing.T) {
	if _, err := New("claude", "", "k", nil).Embed(context.Background(), &schema.EmbedRequest{}); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("err = %v", err)
	}
}
