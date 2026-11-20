package server_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/pricing"
	"github.com/raven-clown/transponder/internal/provider/anthropic"
	oaprovider "github.com/raven-clown/transponder/internal/provider/openai"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
)

// This file only uses what v0.2.4 has as well, so the same benchmark runs on both; see
// docs/benchmarks.md.

// gwTransport answers upstream calls from a handler in the same process.
type gwTransport struct{ h http.Handler }

func (p gwTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	p.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// gwStore holds one key and drops usage, so the benchmark times the gateway and not a disk.
type gwStore struct {
	store.UsageStore
	key store.APIKey
}

func (m *gwStore) CreateKey(context.Context, store.APIKey) error { return nil }

func (m *gwStore) KeyByHash(_ context.Context, hash [32]byte) (store.APIKey, error) {
	if hash != m.key.Hash {
		return store.APIKey{}, store.ErrNotFound
	}
	return m.key, nil
}

func (m *gwStore) HasAdminKey(context.Context, string, time.Time) (bool, error) { return false, nil }

func (m *gwStore) RecordUsage(context.Context, store.UsageRecord) error { return nil }

const (
	gwSystem = "You are a careful assistant for a team that runs a gateway in front of several language model providers. Answer in short plain sentences."
	gwPrompt = "Over the last hour the p99 latency the gateway adds went from 2 ms to 9 ms while traffic stayed flat at about 400 calls a second. How would you tell a slow store from a slow upstream?"
)

var gwText = strings.Repeat("Measure the store write time apart from the upstream time. ", 17)

func gwChat(stream bool) string {
	return fmt.Sprintf(`{"model":"m","stream":%t,"max_tokens":512,"messages":[{"role":"system","content":%q},{"role":"user","content":%q}]}`, stream, gwSystem, gwPrompt)
}

func gwMessages() string {
	return fmt.Sprintf(`{"model":"m","stream":true,"max_tokens":512,"system":%q,"messages":[{"role":"user","content":%q}]}`, gwSystem, gwPrompt)
}

func gwOpenAIAnswer() string {
	return fmt.Sprintf(`{"id":"chatcmpl-bench","object":"chat.completion","created":1700000000,"model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":420,"completion_tokens":256,"total_tokens":676}}`, gwText)
}

func gwOpenAIStream() string {
	var b strings.Builder
	head := `data: {"id":"chatcmpl-bench","object":"chat.completion.chunk","created":1700000000,"model":"gpt-4o-mini","choices":[`
	b.WriteString(head + `{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}` + "\n\n")
	step := len(gwText) / 32
	for i := 0; i < len(gwText); i += step {
		fmt.Fprintf(&b, "%s{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", head, gwText[i:min(i+step, len(gwText))])
	}
	b.WriteString(head + `{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n")
	b.WriteString(head + `],"usage":{"prompt_tokens":420,"completion_tokens":256,"total_tokens":676}}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func gwAnthropicStream() string {
	var b strings.Builder
	ev := func(name, data string) { fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", name, data) }
	ev("message_start", `{"type":"message_start","message":{"id":"msg_bench","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"usage":{"input_tokens":420,"output_tokens":1}}}`)
	ev("ping", `{"type":"ping"}`)
	ev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
	step := len(gwText) / 32
	for i := 0; i < len(gwText); i += step {
		ev("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, gwText[i:min(i+step, len(gwText))]))
	}
	ev("content_block_stop", `{"type":"content_block_stop","index":0}`)
	ev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":256}}`)
	ev("message_stop", `{"type":"message_stop"}`)
	return b.String()
}

func gwEmbeddings() string {
	vector := strings.TrimSuffix(strings.Repeat("0.012345,", 256), ",")
	var data []string
	for i := range 4 {
		data = append(data, fmt.Sprintf(`{"object":"embedding","index":%d,"embedding":[%s]}`, i, vector))
	}
	return `{"object":"list","model":"text-embedding-3-small","data":[` + strings.Join(data, ",") + `],"usage":{"prompt_tokens":32,"total_tokens":32}}`
}

func gwLimiter(b *testing.B) *limits.Limiter {
	p, err := limits.ParseProfile("bench", map[string]any{
		"requests":     map[string]any{"rate": "10000000/m"},
		"llm.tokens":   map[string]any{"rate": "10000000000/m"},
		"llm.cost_usd": map[string]any{"per_day": 1000000},
	})
	if err != nil {
		b.Fatal(err)
	}
	return limits.New(limits.Config{Profiles: []limits.Profile{p}, DefaultProfile: "bench"}, limits.NewMemoryStore(), time.Now)
}

// BenchmarkGateway is one call through the whole gateway for each request path, with a limits
// profile, the key and usage in memory or in SQLite, and the upstream answering in process.
func BenchmarkGateway(b *testing.B) {
	plain, hash, last4, err := auth.NewKey()
	if err != nil {
		b.Fatal(err)
	}
	key := store.APIKey{OrgID: store.DefaultOrg, Name: "bench", Hash: hash, Last4: last4, Role: string(auth.RoleMember), CreatedAt: time.Now()}
	answer := func(contentType, body string) *http.Client {
		return &http.Client{Transport: gwTransport{http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", contentType)
			_, _ = io.WriteString(w, body)
		})}}
	}
	oa := func(c *http.Client, model string) router.Target {
		return router.Target{Provider: oaprovider.New(oaprovider.Options{Name: "oa", BaseURL: "http://up", APIKey: "k", Client: c}), Type: "openai", Model: model}
	}
	an := func(c *http.Client) router.Target {
		return router.Target{Provider: anthropic.New("an", "http://up", "k", c), Type: "anthropic", Model: "claude-sonnet-4-5"}
	}
	embeddings := fmt.Sprintf(`{"model":"m","input":[%q,%q,%q,%q]}`, gwPrompt[:40], gwPrompt[40:80], gwPrompt[80:120], gwSystem[:40])
	cases := []struct {
		name, path, body, expect string
		target                   router.Target
	}{
		{"chat", "/v1/chat/completions", gwChat(false), "chat.completion", oa(answer("application/json", gwOpenAIAnswer()), "gpt-4o-mini")},
		{"chat stream", "/v1/chat/completions", gwChat(true), "[DONE]", oa(answer("text/event-stream", gwOpenAIStream()), "gpt-4o-mini")},
		{"chat stream from anthropic", "/v1/chat/completions", gwChat(true), "[DONE]", an(answer("text/event-stream", gwAnthropicStream()))},
		{"messages stream", "/v1/messages", gwMessages(), "message_stop", an(answer("text/event-stream", gwAnthropicStream()))},
		{"messages stream from openai", "/v1/messages", gwMessages(), "message_stop", oa(answer("text/event-stream", gwOpenAIStream()), "gpt-4o-mini")},
		{"embeddings", "/v1/embeddings", embeddings, `"embedding"`, oa(answer("application/json", gwEmbeddings()), "text-embedding-3-small")},
	}
	ctx := context.Background()
	for _, storage := range []string{"memory", "sqlite"} {
		for _, tt := range cases {
			if storage == "sqlite" && tt.name != "chat" {
				continue
			}
			b.Run(storage+"/"+tt.name, func(b *testing.B) {
				var keys store.KeyStore
				var usage store.UsageStore
				if storage == "sqlite" {
					db, err := sqlite.Open(ctx, filepath.Join(b.TempDir(), "b.db"))
					if err != nil {
						b.Fatal(err)
					}
					defer func() { _ = db.Close() }()
					if err := db.CreateKey(ctx, key); err != nil {
						b.Fatal(err)
					}
					keys, usage = db, db
				} else {
					mem := &gwStore{key: key}
					keys, usage = mem, mem
				}
				h := server.New(server.Deps{
					Auth:    auth.NewAuthenticator(keys, time.Now),
					Router:  router.New([]router.Route{{Match: "*", Targets: []router.Target{tt.target}}}),
					Prices:  pricing.Default(),
					Usage:   usage,
					Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
					Now:     time.Now,
					Limiter: gwLimiter(b),
				})
				b.ReportAllocs()
				for b.Loop() {
					req := httptest.NewRequestWithContext(ctx, http.MethodPost, tt.path, strings.NewReader(tt.body))
					req.Header.Set("Authorization", "Bearer "+plain)
					rec := httptest.NewRecorder()
					h.ServeHTTP(rec, req)
					if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), tt.expect) {
						b.Fatalf("status %d: %s", rec.Code, rec.Body)
					}
				}
			})
		}
	}
}
