package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/schema"
)

const ping = `{"model":"m","messages":[{"role":"user","content":"ping"}]}`

func TestChatCompletions(t *testing.T) {
	e := setup(t, echo.New("echo"))
	res := e.post(t, ping)
	if res.status != http.StatusOK {
		t.Fatalf("status = %d", res.status)
	}
	var body struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(res.body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != "echo: ping" {
		t.Fatalf("body = %s", res.body)
	}
	h := res.header
	if h.Get("X-Usage-Input-Tokens") != "1" || h.Get("X-Usage-Output-Tokens") != "3" || h.Get("X-Usage-Cost-USD") != "0.000000" {
		t.Fatalf("usage headers = %v", h)
	}
	if ct := h.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type = %q", ct)
	}
	recs := e.usage(t)
	if len(recs) != 1 {
		t.Fatalf("records = %d", len(recs))
	}
	r := recs[0]
	if r.User != "alice" || r.Key != "alice" || r.Provider != "echo" || r.Endpoint != "chat.completions" ||
		r.Status != 200 || r.InputTokens != 1 || r.OutputTokens != 3 || r.Model != "m" {
		t.Fatalf("record = %+v", r)
	}
}

func TestChatCompletionsKeepsRequestID(t *testing.T) {
	e := setup(t, echo.New("echo"))
	if _, err := e.do(http.MethodPost, map[string]string{"Authorization": "Bearer " + e.key, "X-Request-ID": "abc-123"}, ping); err != nil {
		t.Fatal(err)
	}
	if got := e.usage(t)[0].RequestID; got != "abc-123" {
		t.Fatalf("request id = %q", got)
	}
}

func TestChatCompletionsUnauthenticated(t *testing.T) {
	e := setup(t, echo.New("echo"))
	res, err := e.do(http.MethodPost, nil, ping)
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusUnauthorized {
		t.Fatalf("status = %d", res.status)
	}
	if res.header.Get("X-Usage-Cost-USD") != "0.000000" {
		t.Fatalf("headers %v", res.header)
	}
	if code := errorCode(t, res); code != "invalid_api_key" {
		t.Fatalf("code = %q", code)
	}
	if n := len(e.usage(t)); n != 0 {
		t.Fatalf("records = %d", n)
	}
}

func TestChatCompletionsBadRequest(t *testing.T) {
	tests := []struct{ name, body, code string }{
		{name: "bad json", body: `{`, code: "invalid_json"},
		{name: "missing model", body: `{"messages":[{"role":"user","content":"x"}]}`, code: "missing_field"},
	}
	e := setup(t, echo.New("echo"))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := e.post(t, tt.body)
			if res.status != http.StatusBadRequest {
				t.Fatalf("status = %d", res.status)
			}
			if code := errorCode(t, res); code != tt.code {
				t.Fatalf("code = %q, want %q", code, tt.code)
			}
		})
	}
}

func TestChatCompletionsBodyTooLarge(t *testing.T) {
	e := setup(t, echo.New("echo"))
	big := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a", 20<<20) + `"}]}`
	if res := e.post(t, big); res.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d", res.status)
	}
}

// failOnReadBody detects attempts to read a request rejected using Content-Length.
type failOnReadBody struct{ t *testing.T }

func (b failOnReadBody) Read([]byte) (int, error) {
	b.t.Error("read oversized request body despite declared Content-Length")
	return 0, errors.New("unexpected request body read")
}

func (failOnReadBody) Close() error { return nil }

func TestChatCompletionsDeclaredBodyTooLargeWithoutReading(t *testing.T) {
	e := setup(t, echo.New("echo"))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", failOnReadBody{t: t})
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.ContentLength = (20 << 20) + 1
	res := httptest.NewRecorder()
	e.srv.Config.Handler.ServeHTTP(res, req)

	got := result{status: res.Code, body: res.Body.Bytes()}
	if got.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", got.status, http.StatusRequestEntityTooLarge)
	}
	if code := errorCode(t, got); code != "body_too_large" {
		t.Fatalf("code = %q, want body_too_large", code)
	}
	if n := len(e.usage(t)); n != 0 {
		t.Fatalf("oversized request produced %d usage records", n)
	}
	if ok := e.post(t, ping); ok.status != http.StatusOK {
		t.Fatalf("normal request status = %d", ok.status)
	}
}

func TestChatCompletionsUnknownLengthBodyTooLarge(t *testing.T) {
	e := setup(t, echo.New("echo"))
	big := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a", 20<<20) + `"}]}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer "+e.key)
	req.ContentLength = -1
	res := httptest.NewRecorder()
	e.srv.Config.Handler.ServeHTTP(res, req)

	got := result{status: res.Code, body: res.Body.Bytes()}
	if got.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", got.status, http.StatusRequestEntityTooLarge)
	}
	if code := errorCode(t, got); code != "body_too_large" {
		t.Fatalf("code = %q, want body_too_large", code)
	}
}

func TestChatCompletionsConcurrent(t *testing.T) {
	e := setup(t, echo.New("echo"))
	var wg sync.WaitGroup
	codes := make(chan int, 20)
	for range 20 {
		wg.Go(func() {
			res, err := e.do(http.MethodPost, map[string]string{"Authorization": "Bearer " + e.key}, ping)
			if err != nil {
				codes <- 0
				return
			}
			codes <- res.status
		})
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusOK {
			t.Fatalf("status = %d", c)
		}
	}
	if n := len(e.usage(t)); n != 20 {
		t.Fatalf("records = %d, want 20", n)
	}
}

func TestChatCompletionsWrongMethod(t *testing.T) {
	e := setup(t, echo.New("echo"))
	res, err := e.do(http.MethodGet, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d", res.status)
	}
}

type failing struct{ *echo.Echo }

func (failing) Chat(context.Context, *schema.ChatRequest) (*schema.ChatResponse, error) {
	return nil, errors.New("connection refused")
}

func TestChatCompletionsProviderFails(t *testing.T) {
	e := setup(t, failing{echo.New("down")})
	res := e.post(t, ping)
	if res.status != http.StatusBadGateway {
		t.Fatalf("status = %d", res.status)
	}
	if code := errorCode(t, res); code != "upstream_error" {
		t.Fatalf("code = %q", code)
	}
	recs := e.usage(t)
	if len(recs) != 1 || recs[0].Status != http.StatusBadGateway || recs[0].Provider != "down" {
		t.Fatalf("records = %+v", recs)
	}
}
