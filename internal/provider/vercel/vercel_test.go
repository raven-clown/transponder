package vercel

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
)

func TestDefaults(t *testing.T) {
	a := New("vercel", "", "k", nil)
	if a.BaseURL() != "https://ai-gateway.vercel.sh/v1" || a.Name() != "vercel" {
		t.Fatalf("adapter %q %q", a.Name(), a.BaseURL())
	}
	const custom = "https://gateway.example/v1"
	if got := New("vercel", custom, "k", nil).BaseURL(); got != custom {
		t.Fatalf("base URL %q", got)
	}
}

func TestForwards(t *testing.T) {
	tests := []struct {
		name   string
		key    string
		header []string
	}{
		{"no key sends no header", "", nil},
		{"key is sent as a bearer token", "secret", []string{"Bearer secret"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			var auth []string
			srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				path, auth = r.URL.Path, r.Header.Values("Authorization")
			}))
			defer srv.Close()
			resp, err := New("vercel", srv.URL+"/v1", tt.key, srv.Client()).Native(context.Background(),
				&provider.NativeCall{Path: "/chat/completions", Body: []byte(`{}`), Header: http.Header{}})
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			if path != "/v1/chat/completions" || !slices.Equal(auth, tt.header) {
				t.Fatalf("path %q, authorization %v", path, auth)
			}
		})
	}
}

// Documented Vercel AI Gateway chat completion, from
// https://vercel.com/docs/ai-gateway/sdks-and-apis/openai-chat-completions/chat-completions.
// total_tokens is prompt_tokens + completion_tokens.
const vercelCompletion = `{"id":"chatcmpl-123","object":"chat.completion","created":1677652288,"model":"anthropic/claude-opus-5","choices":[{"index":0,"message":{"role":"assistant","content":"Once upon a time, a gentle unicorn with a shimmering silver mane danced through moonlit clouds, sprinkling stardust dreams upon sleeping children below."},"finish_reason":"stop"}],"usage":{"prompt_tokens":15,"completion_tokens":28,"total_tokens":43}}`

// Documented reasoning response, from
// https://vercel.com/docs/ai-gateway/sdks-and-apis/openai-chat-completions/reasoning.
// total_tokens is prompt_tokens + completion_tokens, so reasoning_tokens sit inside completion_tokens.
const vercelReasoning = `{"id":"chatcmpl-123","object":"chat.completion","created":1677652288,"model":"openai/gpt-6-astra","choices":[{"index":0,"message":{"role":"assistant","content":"The meaning of life is a deeply personal question...","reasoning":"Let me think about this carefully. The question asks about..."},"finish_reason":"stop"}],"usage":{"prompt_tokens":15,"completion_tokens":150,"total_tokens":165,"completion_tokens_details":{"reasoning_tokens":50}}}`

func TestChatReplaysUsage(t *testing.T) {
	const story = "Once upon a time, a gentle unicorn with a shimmering silver mane danced through moonlit clouds, sprinkling stardust dreams upon sleeping children below."
	tests := []struct {
		name    string
		model   string
		body    string
		content string
		usage   schema.Usage
	}{
		{
			name: "chat completion", model: "anthropic/claude-opus-5", body: vercelCompletion,
			content: story, usage: schema.Usage{InputTokens: 15, OutputTokens: 28},
		},
		{
			name: "reasoning inside completion tokens", model: "openai/gpt-6-astra", body: vercelReasoning,
			content: "The meaning of life is a deeply personal question...",
			usage:   schema.Usage{InputTokens: 15, OutputTokens: 100, ReasoningTokens: 50},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret" {
					t.Errorf("path %s authorization %q", r.URL.Path, r.Header.Get("Authorization"))
				}
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()
			resp, err := New("vercel", srv.URL+"/v1", "secret", srv.Client()).Chat(context.Background(),
				&schema.ChatRequest{Model: tt.model, Messages: []schema.Message{{Role: "user", Content: "hi"}}})
			if err != nil {
				t.Fatal(err)
			}
			if resp.Message.Content != tt.content || resp.Usage != tt.usage {
				t.Fatalf("resp content %q usage %+v", resp.Message.Content, resp.Usage)
			}
		})
	}
}
