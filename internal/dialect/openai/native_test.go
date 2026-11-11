package openai

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/schema"
)

func TestPeekRequest(t *testing.T) {
	tests := []struct {
		body     string
		want     Peek
		wantCode string
	}{
		{body: `{"model":"m","messages":[]}`, want: Peek{Model: "m"}},
		{body: `{"model":"m","stream":true}`, want: Peek{Model: "m", Stream: true}},
		{body: `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`, want: Peek{Model: "m", Stream: true, IncludeUsage: true}},
		{body: `{"model":"m","stream":true,"Stream":false,"Model":"x"}`, want: Peek{Model: "m", Stream: true}},
		{body: `{"model":"m","stream":true,"stream_options":{"include_usage":false,"INCLUDE_USAGE":true}}`, want: Peek{Model: "m", Stream: true}},
		{body: `{"model":"a","model":"m"}`, want: Peek{Model: "m"}},
		{body: `{"model":"m","stream":"yes"}`, wantCode: "invalid_json"},
		{body: `{"model":1}`, wantCode: "invalid_json"},
		{body: `[]`, wantCode: "invalid_json"},
		{body: `{"model":`, wantCode: "invalid_json"},
		{body: `{"stream":true}`, wantCode: "missing_field"},
	}
	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			got, err := PeekRequest([]byte(tt.body))
			if tt.wantCode != "" {
				var re *RequestError
				if !errors.As(err, &re) || re.Code != tt.wantCode {
					t.Fatalf("err = %v, want code %s", err, tt.wantCode)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("PeekRequest = %+v, %v", got, err)
			}
		})
	}
}

func canonicalJSON(t *testing.T, b []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

func TestRewriteRequest(t *testing.T) {
	in := `{"model":"fast","logprobs":true,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"stream_options":{"x":1}}`
	out, err := RewriteRequest([]byte(in), "gpt-4o-mini", true)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"model":"gpt-4o-mini","logprobs":true,"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"stream_options":{"x":1,"include_usage":true}}`
	if canonicalJSON(t, out) != canonicalJSON(t, []byte(want)) {
		t.Fatalf("rewritten %s", out)
	}

	out, err = RewriteRequest([]byte(`{"model":"a"}`), "b", false)
	if err != nil || string(out) != `{"model":"b"}` {
		t.Fatalf("rewrite without usage = %s, %v", out, err)
	}
	out, err = RewriteRequest([]byte(`{"model":"a","Model":"x","stream":true,"STREAM":false}`), "b", false)
	if err != nil || string(out) != `{"model":"b","stream":true}` {
		t.Fatalf("rewrite with case variants = %s, %v", out, err)
	}
	if _, err := RewriteRequest([]byte(`[1]`), "b", false); err == nil {
		t.Fatal("expected an error for a non-object body")
	}
	if _, err := RewriteRequest([]byte(`{"stream_options":3}`), "b", true); err == nil {
		t.Fatal("expected an error for non-object stream_options")
	}
}

func TestParseUsage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want schema.Usage
		ok   bool
	}{
		{"chat with cache", `{"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":200}}}`, schema.Usage{InputTokens: 800, CacheReadTokens: 200, OutputTokens: 50}, true},
		{"embeddings", `{"usage":{"prompt_tokens":12,"total_tokens":12}}`, schema.Usage{InputTokens: 12}, true},
		{"no usage", `{"choices":[]}`, schema.Usage{}, false},
		{"null usage", `{"usage":null}`, schema.Usage{}, false},
		{"not json", `nope`, schema.Usage{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseUsage([]byte(tt.body))
			if got != tt.want || ok != tt.ok {
				t.Fatalf("ParseUsage = %+v, %v", got, ok)
			}
		})
	}
}

func TestParseChatRequest(t *testing.T) {
	req, err := ParseChatRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil || req.Model != "m" || req.Messages[0].Content != "hi" {
		t.Fatalf("ParseChatRequest = %+v, %v", req, err)
	}
}

func TestParseUsageReasoning(t *testing.T) {
	u, ok := ParseUsage([]byte(`{"usage":{"prompt_tokens":10,"completion_tokens":100,"total_tokens":110,"completion_tokens_details":{"reasoning_tokens":60}}}`))
	if !ok || u.OutputTokens != 40 || u.ReasoningTokens != 60 {
		t.Fatalf("usage %+v", u)
	}
	// Documented xAI chat completion: reasoning is not inside completion_tokens.
	// https://docs.x.ai/developers/advanced-api-usage/deferred-chat-completions
	xai := []byte(`{"id":"3f4ddfca-b997-3bd4-80d4-8112278a1508","choices":[{"message":{"role":"assistant","content":"42"},"finish_reason":"stop"}],"usage":{"prompt_tokens":26,"completion_tokens":168,"total_tokens":498,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":304}}}`)
	u, ok = ParseUsage(xai)
	if !ok || u.InputTokens != 22 || u.CacheReadTokens != 4 || u.OutputTokens != 168 || u.ReasoningTokens != 304 {
		t.Fatalf("xai native usage %+v", u)
	}
	resp, err := DecodeChatResponse(xai)
	if err != nil || resp.Usage.InputTokens != 22 || resp.Usage.CacheReadTokens != 4 || resp.Usage.OutputTokens != 168 || resp.Usage.ReasoningTokens != 304 {
		t.Fatalf("xai translated usage %+v %v", resp, err)
	}
	cu, cok, _ := ChunkUsage(xai)
	if !cok || cu != u {
		t.Fatalf("xai chunk usage %+v", cu)
	}

	b, err := EncodeChatResponse(&schema.ChatResponse{Usage: schema.Usage{InputTokens: 10, OutputTokens: 40, ReasoningTokens: 60}}, time.Unix(1, 0))
	if err != nil || !strings.Contains(string(b), `"completion_tokens":100,"total_tokens":110`) || !strings.Contains(string(b), `"completion_tokens_details":{"reasoning_tokens":60}`) {
		t.Fatalf("encoded %s %v", b, err)
	}
}
