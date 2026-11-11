package xai

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
	a := New("xai", "", "k", nil)
	if a.BaseURL() != "https://api.x.ai/v1" || a.Name() != "xai" {
		t.Fatalf("adapter %q %q", a.Name(), a.BaseURL())
	}
	const custom = "https://us.api.x.ai/v1"
	if got := New("xai", custom, "k", nil).BaseURL(); got != custom {
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
			resp, err := New("xai", srv.URL+"/v1", tt.key, srv.Client()).Native(context.Background(),
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

// Documented xAI chat completion usage, from
// https://docs.x.ai/developers/advanced-api-usage/deferred-chat-completions.
// reasoning_tokens sit outside completion_tokens: 26+168+304 = 498.
const xaiCompletion = `{"id":"3f4ddfca-b997-3bd4-80d4-8112278a1508","object":"chat.completion","model":"grok-4.7","choices":[{"index":0,"message":{"role":"assistant","content":"42"},"finish_reason":"stop"}],"usage":{"prompt_tokens":26,"completion_tokens":168,"total_tokens":498,"prompt_tokens_details":{"text_tokens":26,"audio_tokens":0,"image_tokens":0,"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":304,"audio_tokens":0,"accepted_prediction_tokens":0,"rejected_prediction_tokens":0},"num_sources_used":0}}`

func TestChatReplaysUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("path %s authorization %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, xaiCompletion)
	}))
	defer srv.Close()
	resp, err := New("xai", srv.URL+"/v1", "secret", srv.Client()).Chat(context.Background(),
		&schema.ChatRequest{Model: "grok-4.7", Messages: []schema.Message{{Role: "user", Content: "126/3=?"}}})
	if err != nil {
		t.Fatal(err)
	}
	u := resp.Usage
	if resp.Message.Content != "42" || u.InputTokens != 22 || u.CacheReadTokens != 4 || u.OutputTokens != 168 || u.ReasoningTokens != 304 {
		t.Fatalf("resp content %q usage %+v", resp.Message.Content, u)
	}
}
