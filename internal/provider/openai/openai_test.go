package openai

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

func TestCanonicalUnsupported(t *testing.T) {
	a := New(Options{Name: "openai"})
	ctx := context.Background()
	if _, err := a.Chat(ctx, &schema.ChatRequest{}); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("Chat err = %v", err)
	}
	if _, err := a.ChatStream(ctx, &schema.ChatRequest{}); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("ChatStream err = %v", err)
	}
	if _, err := a.Embed(ctx, &schema.EmbedRequest{}); !errors.Is(err, provider.ErrUnsupported) {
		t.Fatalf("Embed err = %v", err)
	}
}
