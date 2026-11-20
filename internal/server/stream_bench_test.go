package server_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/pricing"
	"github.com/raven-clown/transponder/internal/provider/anthropic"
	oaprovider "github.com/raven-clown/transponder/internal/provider/openai"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/store"
)

// inProcess serves upstream calls from a handler in the same process, so a benchmark times
// the gateway and not the network.
type inProcess struct{ h http.Handler }

func (p inProcess) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	p.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// BenchmarkCall is one call through the gateway for each request path: native and translated,
// streamed and not, with the key and usage in memory and no metrics.
func BenchmarkCall(b *testing.B) {
	plain, hash, last4, err := auth.NewKey()
	if err != nil {
		b.Fatal(err)
	}
	key := store.APIKey{OrgID: store.DefaultOrg, Name: "bench", Hash: hash, Last4: last4, Role: string(auth.RoleMember), CreatedAt: time.Now()}
	answer := func(contentType, body string) *http.Client {
		return &http.Client{Transport: inProcess{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", contentType)
			_, _ = io.WriteString(w, body)
		})}}
	}
	oa := func(c *http.Client) router.Target {
		return router.Target{Provider: oaprovider.New(oaprovider.Options{Name: "oa", BaseURL: "http://up", APIKey: "k", Client: c}), Type: "openai", Model: "gpt-4o-mini"}
	}
	an := func(c *http.Client) router.Target {
		return router.Target{Provider: anthropic.New("an", "http://up", "k", c), Type: "anthropic", Model: "claude-sonnet-4-5"}
	}
	stream, err := os.ReadFile("testdata/responses/stream.sse")
	if err != nil {
		b.Fatal(err)
	}
	responses := string(stream)
	anStream := anthropicStream + "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	cases := []struct {
		name, path, body string
		target           router.Target
	}{
		{"chat", "/v1/chat/completions", ping, oa(answer("application/json", okChat))},
		{"chat stream", "/v1/chat/completions", streamPing, oa(answer("text/event-stream", upstreamChunks))},
		{"chat stream from anthropic", "/v1/chat/completions", streamPing, an(answer("text/event-stream", anStream))},
		{"messages stream", "/v1/messages", `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"ping"}]}`, an(answer("text/event-stream", anStream))},
		{"messages stream from openai", "/v1/messages", `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"ping"}]}`, oa(answer("text/event-stream", upstreamChunks))},
		{"responses stream", "/v1/responses", responsesStream, oa(answer("text/event-stream", responses))},
	}
	for _, tt := range cases {
		b.Run(tt.name, func(b *testing.B) {
			mem := &memoryStore{key: key}
			h := server.New(server.Deps{
				Auth:   auth.NewAuthenticator(mem, time.Now),
				Router: router.New([]router.Route{{Match: "*", Targets: []router.Target{tt.target}}}),
				Prices: pricing.Default(),
				Usage:  mem,
				Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
				Now:    time.Now,
			})
			b.ReportAllocs()
			for b.Loop() {
				req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, tt.path, strings.NewReader(tt.body))
				req.Header.Set("Authorization", "Bearer "+plain)
				rec := httptest.NewRecorder()
				h.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "upstream_error") {
					b.Fatalf("status %d: %s", rec.Code, rec.Body)
				}
			}
		})
	}
}
