package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// upstreamConfig fixes what the fake provider does on every call, so two runs differ only in
// the gateway in front of it.
type upstreamConfig struct {
	// Latency passes before the first byte of an answer.
	Latency time.Duration
	// Size is the length of the answer text in bytes.
	Size int
	// Chunks is how many stream events carry the answer text.
	Chunks int
	// Interval passes between stream events.
	Interval time.Duration
	// Dims is the length of each embedding.
	Dims int
}

func defaultUpstream() upstreamConfig {
	return upstreamConfig{Latency: 20 * time.Millisecond, Size: 1024, Chunks: 32, Dims: 256}
}

// Usage the fake provider reports on every chat call.
const (
	promptTokens     = 250
	completionTokens = 256
)

// upstream answers OpenAI chat completions, plain and streamed, and embeddings with bodies
// built once at start.
type upstream struct {
	cfg    upstreamConfig
	chat   []byte
	events [][]byte
	vector string
}

func newUpstream(cfg upstreamConfig) (*upstream, error) {
	if cfg.Latency < 0 || cfg.Interval < 0 || cfg.Size < 1 || cfg.Chunks < 1 || cfg.Dims < 1 {
		return nil, errors.New("upstream: latency and interval must not be negative, size, chunks and dims must be positive")
	}
	u := &upstream{cfg: cfg}
	text := strings.Repeat("bench ", cfg.Size/6+1)[:cfg.Size]
	usage := map[string]int{"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "total_tokens": promptTokens + completionTokens}
	var err error
	u.chat, err = json.Marshal(map[string]any{
		"id": "chatcmpl-bench", "object": "chat.completion", "created": 1700000000, "model": "bench",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": text}}},
		"usage":   usage,
	})
	if err != nil {
		return nil, err
	}
	event := func(choices []any, usage any) error {
		chunk := map[string]any{"id": "chatcmpl-bench", "object": "chat.completion.chunk", "created": 1700000000, "model": "bench", "choices": choices}
		if usage != nil {
			chunk["usage"] = usage
		}
		b, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		u.events = append(u.events, []byte("data: "+string(b)+"\n\n"))
		return nil
	}
	delta := func(d map[string]any, finish any) []any {
		return []any{map[string]any{"index": 0, "delta": d, "finish_reason": finish}}
	}
	if err := event(delta(map[string]any{"role": "assistant", "content": ""}, nil), nil); err != nil {
		return nil, err
	}
	step := (len(text) + cfg.Chunks - 1) / cfg.Chunks
	for i := 0; i < len(text); i += step {
		if err := event(delta(map[string]any{"content": text[i:min(i+step, len(text))]}, nil), nil); err != nil {
			return nil, err
		}
	}
	if err := event(delta(map[string]any{}, "stop"), nil); err != nil {
		return nil, err
	}
	if err := event([]any{}, usage); err != nil {
		return nil, err
	}
	u.events = append(u.events, []byte("data: [DONE]\n\n"))
	vector := make([]string, cfg.Dims)
	for i := range vector {
		vector[i] = strconv.FormatFloat(float64(i%97)/97-0.5, 'f', 6, 64)
	}
	u.vector = "[" + strings.Join(vector, ",") + "]"
	return u, nil
}

func (u *upstream) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", u.chatHandler)
	mux.HandleFunc("POST /v1/embeddings", u.embeddingsHandler)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	return mux
}

// wait sleeps d and reports whether the caller is still there.
func wait(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type benchRequest struct {
	Stream bool            `json:"stream"`
	Input  json.RawMessage `json:"input"`
}

func decode(w http.ResponseWriter, r *http.Request) (benchRequest, bool) {
	var req benchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"the body must be JSON","type":"invalid_request_error"}}`)
		return req, false
	}
	return req, true
}

func (u *upstream) chatHandler(w http.ResponseWriter, r *http.Request) {
	req, ok := decode(w, r)
	if !ok || !wait(r.Context(), u.cfg.Latency) {
		return
	}
	if !req.Stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(u.chat)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)
	for i, e := range u.events {
		if i > 0 && !wait(r.Context(), u.cfg.Interval) {
			return
		}
		_, _ = w.Write(e)
		_ = rc.Flush()
	}
}

func (u *upstream) embeddingsHandler(w http.ResponseWriter, r *http.Request) {
	req, ok := decode(w, r)
	if !ok {
		return
	}
	n := 1
	var list []json.RawMessage
	if json.Unmarshal(req.Input, &list) == nil && len(list) > 0 {
		n = len(list)
	}
	if !wait(r.Context(), u.cfg.Latency) {
		return
	}
	var b bytes.Buffer
	b.WriteString(`{"object":"list","model":"bench","data":[`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"object":"embedding","index":%d,"embedding":%s}`, i, u.vector)
	}
	fmt.Fprintf(&b, `],"usage":{"prompt_tokens":%d,"total_tokens":%d}}`, 8*n, 8*n)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b.Bytes())
}

func upstreamFlags(fs *flag.FlagSet, cfg *upstreamConfig) {
	fs.DurationVar(&cfg.Latency, "latency", cfg.Latency, "time before the first byte of each answer")
	fs.IntVar(&cfg.Size, "size", cfg.Size, "answer text length in bytes")
	fs.IntVar(&cfg.Chunks, "chunks", cfg.Chunks, "stream events that carry the answer text")
	fs.DurationVar(&cfg.Interval, "interval", cfg.Interval, "time between stream events")
	fs.IntVar(&cfg.Dims, "dims", cfg.Dims, "embedding length")
}

func runUpstream(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("upstream", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", "127.0.0.1:9000", "address to listen on")
	cfg := defaultUpstream()
	upstreamFlags(fs, &cfg)
	if err := fs.Parse(args); err != nil {
		return err
	}
	u, err := newUpstream(cfg)
	if err != nil {
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *addr)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "upstream: listening on %s\n", ln.Addr())
	return serve(ctx, ln, u.handler())
}

// serve serves h on ln until ctx ends.
func serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	_ = srv.Close()
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
