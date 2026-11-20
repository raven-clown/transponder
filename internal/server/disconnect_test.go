package server_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
)

// logBuffer collects log lines from handlers that run on other goroutines.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor fails the test when ch is not closed within a few seconds.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// TestCallerDisconnect checks #25: a caller that leaves, before the answer or in the middle of
// a stream, is recorded as 499 and logged at info level, and the upstream call is cancelled.
func TestCallerDisconnect(t *testing.T) {
	const (
		oaChunk  = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"
		anStart  = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":7}}}\n\n"
		resStart = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\"}}\n\n"
	)
	cases := []struct {
		name  string
		path  string
		body  string
		first string // what the upstream sends before it waits; empty for no answer at all
		route func(up string, c *counted) router.Route
	}{
		{"chat plain", "/v1/chat/completions", ping, "", oaRoute},
		{"chat stream native", "/v1/chat/completions", streamPing, oaChunk, oaRoute},
		{"chat stream translated", "/v1/chat/completions", streamPing, anStart + anTextDelta("hi"), anRoute},
		{"messages plain", "/v1/messages", messagesPing, "", anRoute},
		{"messages stream native", "/v1/messages", messagesStreamPing, anStart + anTextDelta("hi"), anRoute},
		{"messages stream translated", "/v1/messages", messagesStreamPing, oaChunk, oaRoute},
		{"responses plain", "/v1/responses", responsesPing, "", oaRoute},
		{"responses stream", "/v1/responses", responsesStream, resStart, oaRoute},
		{"embeddings", "/v1/embeddings", `{"model":"m","input":"x"}`, "", oaRoute},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			arrived, gone := make(chan struct{}), make(chan struct{})
			up := countedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				// A server sees its caller hang up only once it has read the body.
				_, _ = io.Copy(io.Discard, r.Body)
				if tt.first != "" {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, tt.first)
					w.(http.Flusher).Flush()
				}
				close(arrived)
				<-r.Context().Done()
				close(gone)
			})
			logs := &logBuffer{}
			e := setupDeps(t, []router.Route{tt.route("*", up)}, func(d *server.Deps) {
				d.Logger = slog.New(slog.NewTextHandler(logs, nil))
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.srv.URL+tt.path, strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+e.key)
			req.Header.Set("X-Api-Key", e.key)
			done := make(chan struct{})
			go func() {
				defer close(done)
				resp, err := e.srv.Client().Do(req)
				if err != nil {
					return
				}
				defer func() { _ = resp.Body.Close() }()
				if tt.first != "" {
					// Read the first frame, so the stream has begun before the caller leaves.
					_, _ = resp.Body.Read(make([]byte, 1))
				}
				cancel()
				_, _ = io.Copy(io.Discard, resp.Body)
			}()
			waitFor(t, arrived, "the upstream call")
			if tt.first == "" {
				cancel()
			}
			waitFor(t, done, "the caller to leave")
			waitFor(t, gone, "the cancel to reach the upstream")
			e.srv.Close()
			recs := e.usage(t)
			if len(recs) != 1 || recs[0].Status != 499 {
				t.Fatalf("records %+v, want one with status 499", recs)
			}
			if out := logs.String(); strings.Contains(out, "level=WARN") {
				t.Fatalf("a caller leaving was logged as a warning:\n%s", out)
			}
		})
	}
}

const messagesStreamPing = `{"model":"m","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"ping"}]}`

func anTextDelta(text string) string {
	return "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"" + text + "\"}}\n\n"
}

func oaRoute(match string, up *counted) router.Route {
	return router.Route{Match: match, Targets: []router.Target{oaTarget("oa", "", up)}}
}

func anRoute(match string, up *counted) router.Route {
	return router.Route{Match: match, Targets: []router.Target{anTarget("", up)}}
}
