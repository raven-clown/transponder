package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
)

func fake(t *testing.T, wantPath, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != wantPath || r.Header.Get("X-Goog-Api-Key") != "gk" {
			t.Errorf("request %s, key %q", r.URL.RequestURI(), r.Header.Get("X-Goog-Api-Key"))
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestDefaults(t *testing.T) {
	a := New("g", "", "gk", nil)
	if a.BaseURL() != "https://generativelanguage.googleapis.com/v1beta" || a.Dialect() != "gemini" || a.Name() != "g" {
		t.Fatalf("adapter %q %q", a.BaseURL(), a.Dialect())
	}
}

func TestChat(t *testing.T) {
	srv := fake(t, "/models/gemini-2.5-flash:generateContent", `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1}}`)
	resp, err := New("g", srv.URL, "gk", nil).Chat(context.Background(), &schema.ChatRequest{Model: "gemini-2.5-flash", Messages: []schema.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "hi" || resp.Model != "gemini-2.5-flash" || resp.Usage.InputTokens != 3 {
		t.Fatalf("resp %+v", resp)
	}
}

func TestChatStream(t *testing.T) {
	srv := fake(t, "/models/m:streamGenerateContent?alt=sse",
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\r\n\r\n"+
			"data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3}}\r\n\r\n")
	st, err := New("g", srv.URL, "gk", nil).ChatStream(context.Background(), &schema.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	n := 0
	for {
		_, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 4 {
		t.Fatalf("%d events", n)
	}
}

func TestChatStreamCut(t *testing.T) {
	srv := fake(t, "/models/m:streamGenerateContent?alt=sse", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n\n")
	st, err := New("g", srv.URL, "gk", nil).ChatStream(context.Background(), &schema.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for {
		_, err := st.Recv()
		if err == nil {
			continue
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("err = %v", err)
		}
		return
	}
}

func TestEmbed(t *testing.T) {
	srv := fake(t, "/models/gemini-embedding-001:batchEmbedContents", `{"embeddings":[{"values":[0.5]}]}`)
	resp, err := New("g", srv.URL, "gk", nil).Embed(context.Background(), &schema.EmbedRequest{Model: "gemini-embedding-001", Input: []string{"abcd"}})
	if err != nil || len(resp.Vectors) != 1 || resp.Usage.InputTokens != 1 {
		t.Fatalf("resp %+v %v", resp, err)
	}
}

func TestErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer srv.Close()
	a := New("g", srv.URL, "gk", nil)
	ctx := context.Background()
	var pe *provider.Error
	if _, err := a.Chat(ctx, &schema.ChatRequest{Model: "m"}); !errors.As(err, &pe) || pe.Status != 429 {
		t.Fatalf("Chat err = %v", err)
	}
	if _, err := a.ChatStream(ctx, &schema.ChatRequest{Model: "m"}); !errors.As(err, &pe) {
		t.Fatalf("ChatStream err = %v", err)
	}
	if _, err := a.Embed(ctx, &schema.EmbedRequest{Model: "m"}); !errors.As(err, &pe) {
		t.Fatalf("Embed err = %v", err)
	}
}

func TestRejectsUnsafeModelNames(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer srv.Close()
	a := New("g", srv.URL, "gk", nil)
	ctx := context.Background()
	bad := "x:batchEmbedContents?"
	if _, err := a.Chat(ctx, &schema.ChatRequest{Model: bad}); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("Chat err = %v", err)
	}
	if _, err := a.ChatStream(ctx, &schema.ChatRequest{Model: "../files"}); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("ChatStream err = %v", err)
	}
	if _, err := a.Embed(ctx, &schema.EmbedRequest{Model: "a/b"}); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("Embed err = %v", err)
	}
	if called {
		t.Fatal("an unsafe model name reached the upstream")
	}
}
