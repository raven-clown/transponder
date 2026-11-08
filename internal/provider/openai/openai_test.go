package openai

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

func call(body string) *provider.NativeCall {
	h := http.Header{}
	h.Set("X-Request-ID", "req-1")
	h.Set("Traceparent", "00-abc-def-01")
	return &provider.NativeCall{Path: "/chat/completions", Body: []byte(body), Header: h}
}

func TestNativeForwards(t *testing.T) {
	var got *http.Request
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()

	a := New(Options{Name: "openai", BaseURL: srv.URL + "/v1/", APIKey: "k"})
	resp, err := a.Native(context.Background(), call(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"ok":true}` {
		t.Fatalf("body %q", body)
	}
	switch {
	case got.Method != http.MethodPost, got.URL.Path != "/v1/chat/completions":
		t.Fatalf("request %s %s", got.Method, got.URL.Path)
	case string(gotBody) != `{"model":"m"}`:
		t.Fatalf("upstream body %q", gotBody)
	case got.Header.Get("Authorization") != "Bearer k":
		t.Fatalf("authorization %q", got.Header.Get("Authorization"))
	case got.Header.Get("Content-Type") != "application/json":
		t.Fatalf("content type %q", got.Header.Get("Content-Type"))
	case got.Header.Get("X-Request-ID") != "req-1", got.Header.Get("Traceparent") != "00-abc-def-01":
		t.Fatalf("trace headers %v", got.Header)
	}
}

func TestNativeNoKey(t *testing.T) {
	var auth []string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		auth = r.Header.Values("Authorization")
	}))
	defer srv.Close()
	resp, err := New(Options{Name: "local", BaseURL: srv.URL}).Native(context.Background(), call(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(auth) != 0 {
		t.Fatalf("authorization sent: %v", auth)
	}
}

type closeTracker struct {
	io.ReadCloser
	closed *bool
}

func (c closeTracker) Close() error {
	*c.closed = true
	return c.ReadCloser.Close()
}

type trackingTransport struct {
	closed *bool
}

func (t trackingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil {
		resp.Body = closeTracker{resp.Body, t.closed}
	}
	return resp, err
}

func TestNativeErrors(t *testing.T) {
	for _, status := range []int{400, 401, 429, 500, 529} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"error":{"message":"nope"}}`)
		}))
		closed := false
		a := New(Options{Name: "openai", BaseURL: srv.URL, Client: &http.Client{Transport: trackingTransport{&closed}}})
		resp, err := a.Native(context.Background(), call(`{}`))
		srv.Close()
		if resp != nil {
			_ = resp.Body.Close()
			t.Fatalf("%d: got a response", status)
		}
		var pe *provider.Error
		if !errors.As(err, &pe) {
			t.Fatalf("%d: err = %v", status, err)
		}
		if pe.Status != status || pe.Retryable != provider.RetryableStatus(status) || string(pe.Body) != `{"error":{"message":"nope"}}` || pe.Provider != "openai" {
			t.Fatalf("%d: error %+v", status, pe)
		}
		if !closed {
			t.Fatalf("%d: body not closed", status)
		}
	}
}

func TestNativeConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	resp, err := New(Options{Name: "openai", BaseURL: url}).Native(context.Background(), call(`{}`))
	if resp != nil {
		_ = resp.Body.Close()
	}
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Status != 0 || !pe.Retryable {
		t.Fatalf("err = %v", err)
	}
}

func TestBaseURLDefault(t *testing.T) {
	a := New(Options{Name: "openai"})
	if a.BaseURL() != "https://api.openai.com/v1" || a.Name() != "openai" || a.Dialect() != "openai" {
		t.Fatalf("adapter %q %q %q", a.BaseURL(), a.Name(), a.Dialect())
	}
}

func TestChat(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"id":"r","model":"gpt-x","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	a := New(Options{Name: "oa", BaseURL: srv.URL, APIKey: "k"})
	resp, err := a.Chat(context.Background(), &schema.ChatRequest{Model: "gpt-x", Messages: []schema.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Message.Content != "hi" || resp.FinishReason != "stop" || resp.Usage.InputTokens != 3 || got["model"] != "gpt-x" {
		t.Fatalf("resp %+v, upstream got %v", resp, got)
	}
}

func TestChatStream(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"+
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"+
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":1}}\n\n"+
			"data: [DONE]\n\n")
	}))
	defer srv.Close()
	st, err := New(Options{Name: "oa", BaseURL: srv.URL}).ChatStream(context.Background(), &schema.ChatRequest{Model: "m", Messages: []schema.Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var types []schema.EventType
	for {
		ev, err := st.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		types = append(types, ev.Type)
	}
	want := []schema.EventType{schema.EventStart, schema.EventText, schema.EventFinish, schema.EventUsage}
	if !reflect.DeepEqual(types, want) || got["stream"] != true {
		t.Fatalf("events %v, upstream got %v", types, got)
	}
}

func TestChatStreamCut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
	}))
	defer srv.Close()
	st, err := New(Options{Name: "oa", BaseURL: srv.URL}).ChatStream(context.Background(), &schema.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	for {
		_, err := st.Recv()
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			t.Fatal("a stream without [DONE] ended cleanly")
		}
		return
	}
}

func TestEmbed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"model":"e","data":[{"index":0,"embedding":[0.5]}],"usage":{"prompt_tokens":2}}`)
	}))
	defer srv.Close()
	resp, err := New(Options{Name: "oa", BaseURL: srv.URL}).Embed(context.Background(), &schema.EmbedRequest{Model: "e", Input: []string{"a"}})
	if err != nil || len(resp.Vectors) != 1 || resp.Usage.InputTokens != 2 {
		t.Fatalf("resp %+v, %v", resp, err)
	}
}

func TestCanonicalErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	a := New(Options{Name: "oa", BaseURL: srv.URL})
	ctx := context.Background()
	var pe *provider.Error
	if _, err := a.Chat(ctx, &schema.ChatRequest{Model: "m"}); !errors.As(err, &pe) || pe.Status != 429 {
		t.Fatalf("Chat err = %v", err)
	}
	if _, err := a.ChatStream(ctx, &schema.ChatRequest{Model: "m"}); !errors.As(err, &pe) || pe.Status != 429 {
		t.Fatalf("ChatStream err = %v", err)
	}
	if _, err := a.Embed(ctx, &schema.EmbedRequest{Model: "m"}); !errors.As(err, &pe) || pe.Status != 429 {
		t.Fatalf("Embed err = %v", err)
	}
}

func TestCanonicalForwardsHeaders(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Request-ID")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	}))
	defer srv.Close()
	ctx := provider.WithHeader(context.Background(), http.Header{"X-Request-Id": {"req-9"}})
	if _, err := New(Options{Name: "oa", BaseURL: srv.URL}).Chat(ctx, &schema.ChatRequest{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if got != "req-9" {
		t.Fatalf("X-Request-ID %q", got)
	}
}

func TestMaxCompletionTokens(t *testing.T) {
	for _, useCompletion := range []bool{true, false} {
		var got map[string]json.RawMessage
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &got)
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
		}))
		n := 32000
		_, err := New(Options{Name: "oa", BaseURL: srv.URL, MaxCompletionTokens: useCompletion}).Chat(context.Background(), &schema.ChatRequest{Model: "gpt-5", MaxTokens: &n})
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		_, hasMax := got["max_tokens"]
		_, hasCompletion := got["max_completion_tokens"]
		if hasCompletion != useCompletion || hasMax == useCompletion {
			t.Fatalf("MaxCompletionTokens=%v: upstream body %v", useCompletion, got)
		}
	}
}
