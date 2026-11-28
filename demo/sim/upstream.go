package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"time"
)

// answer is what the fake provider says, whatever it is asked.
const answer = "This is a canned answer from the demo upstream. It has no model behind it, only a timer and a counter."

// upstream answers chat completions, Responses API calls and embeddings in the OpenAI wire
// format, after a delay, and fails some of the time, so the gateway in front of it has
// latency, tokens and fallbacks to show.
type upstream struct {
	minLatency, maxLatency time.Duration
	failRate               float64
	// random returns a number in [0, 1). Tests replace it.
	random func() float64
}

func runUpstream(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("upstream", flag.ContinueOnError)
	fs.SetOutput(stderr)
	addr := fs.String("addr", ":9000", "address to listen on")
	minLatency := fs.Duration("min-latency", 80*time.Millisecond, "shortest time before an answer")
	maxLatency := fs.Duration("max-latency", 600*time.Millisecond, "longest time before an answer")
	failRate := fs.Float64("fail", 0, "share of calls answered with 503, from 0 to 1")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *failRate < 0 || *failRate > 1 || *minLatency < 0 || *maxLatency < *minLatency {
		return errors.New("upstream: -fail must be between 0 and 1, and -max-latency at least -min-latency")
	}
	u := &upstream{minLatency: *minLatency, maxLatency: *maxLatency, failRate: *failRate, random: rand.Float64}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", *addr)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "upstream: listening on %s, failing %.0f%% of calls\n", ln.Addr(), *failRate*100)
	return serveUntilDone(ctx, ln, u.handler())
}

// serveUntilDone serves h on ln until ctx ends.
func serveUntilDone(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	_ = srv.Close()
	// Serve returns ErrServerClosed once Close has run; that is the expected end.
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (u *upstream) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", u.chat)
	mux.HandleFunc("POST /v1/responses", u.responses)
	mux.HandleFunc("POST /v1/embeddings", u.embeddings)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	return mux
}

// begin waits as a provider would and reports whether the call goes on. It answers 503
// itself for the share of calls that fail.
func (u *upstream) begin(w http.ResponseWriter, r *http.Request) bool {
	wait := u.minLatency + time.Duration(u.random()*float64(u.maxLatency-u.minLatency))
	select {
	case <-time.After(wait):
	case <-r.Context().Done():
		return false
	}
	if u.random() < u.failRate {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]any{"message": "the demo upstream is pretending to be overloaded", "type": "server_error"},
		})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func badRequest(w http.ResponseWriter, message string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error": map[string]any{"message": message, "type": "invalid_request_error"},
	})
}

type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// tokens is a rough count, a token for every four bytes, which is all a demo needs.
func tokens(n int) int { return n/4 + 1 }

// words returns the answer cut at a random word, so output tokens vary from call to call.
func (u *upstream) words() []string {
	words := strings.Fields(answer)
	return words[:1+int(u.random()*float64(len(words)))]
}

func (u *upstream) chat(w http.ResponseWriter, r *http.Request) {
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.Model == "" {
		badRequest(w, "the body must be a JSON chat request with a model")
		return
	}
	if !u.begin(w, r) {
		return
	}
	prompt := 0
	for _, m := range req.Messages {
		prompt += len(m.Content)
	}
	words := u.words()
	text := strings.Join(words, " ")
	use := map[string]int{"prompt_tokens": tokens(prompt), "completion_tokens": tokens(len(text))}
	use["total_tokens"] = use["prompt_tokens"] + use["completion_tokens"]
	id, created := fmt.Sprintf("chatcmpl-demo-%08x", int64(u.random()*1e9)), time.Now().Unix()
	if !req.Stream {
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "object": "chat.completion", "created": created, "model": req.Model,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": text},
			}},
			"usage": use,
		})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)
	send := func(choices []any, usage any) {
		chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": req.Model, "choices": choices}
		if usage != nil {
			chunk["usage"] = usage
		}
		b, _ := json.Marshal(chunk)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		_ = rc.Flush()
	}
	delta := func(d map[string]any, finish any) []any {
		return []any{map[string]any{"index": 0, "delta": d, "finish_reason": finish}}
	}
	send(delta(map[string]any{"role": "assistant", "content": ""}, nil), nil)
	for i, word := range words {
		if i > 0 {
			word = " " + word
		}
		send(delta(map[string]any{"content": word}, nil), nil)
		select {
		case <-time.After(15 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
	send(delta(map[string]any{}, "stop"), nil)
	send([]any{}, use)
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	_ = rc.Flush()
}

func (u *upstream) embeddings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.Model == "" {
		badRequest(w, "the body must be a JSON embeddings request with a model")
		return
	}
	// The input is one string or a list of them.
	var inputs []string
	var one string
	switch {
	case json.Unmarshal(req.Input, &one) == nil:
		inputs = []string{one}
	case json.Unmarshal(req.Input, &inputs) == nil && len(inputs) > 0:
	default:
		badRequest(w, "input must be a string or a list of strings")
		return
	}
	if !u.begin(w, r) {
		return
	}
	data := make([]any, len(inputs))
	prompt := 0
	for i, in := range inputs {
		prompt += tokens(len(in))
		vector := make([]float64, 8)
		for j := range vector {
			vector[j] = u.random()*2 - 1
		}
		data[i] = map[string]any{"object": "embedding", "index": i, "embedding": vector}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list", "model": req.Model, "data": data,
		"usage": map[string]int{"prompt_tokens": prompt, "total_tokens": prompt},
	})
}

type responsesRequest struct {
	Model  string          `json:"model"`
	Stream bool            `json:"stream"`
	Input  json.RawMessage `json:"input"`
}

// responseObject is a Responses API response with the fields a client reads.
func responseObject(id, model string, created int64, status string, output []any, usage any) map[string]any {
	return map[string]any{
		"id": id, "object": "response", "created_at": created, "status": status, "error": nil,
		"model": model, "output": output, "usage": usage,
	}
}

func (u *upstream) responses(w http.ResponseWriter, r *http.Request) {
	var req responsesRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || req.Model == "" {
		badRequest(w, "the body must be a JSON Responses request with a model")
		return
	}
	if !u.begin(w, r) {
		return
	}
	words := u.words()
	text := strings.Join(words, " ")
	input := tokens(len(req.Input))
	output := tokens(len(text))
	// Cached and reasoning tokens are part of usage on the newer models, so the demo shows them.
	use := map[string]any{
		"input_tokens": input, "input_tokens_details": map[string]int{"cached_tokens": int(u.random() * float64(input))},
		"output_tokens": output, "output_tokens_details": map[string]int{"reasoning_tokens": output / 4},
		"total_tokens": input + output,
	}
	id := fmt.Sprintf("resp_demo_%08x", int64(u.random()*1e9))
	msgID := "msg_" + strings.TrimPrefix(id, "resp_")
	created := time.Now().Unix()
	message := map[string]any{
		"type": "message", "id": msgID, "status": "completed", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	if !req.Stream {
		writeJSON(w, http.StatusOK, responseObject(id, req.Model, created, "completed", []any{message}, use))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)
	seq := 0
	send := func(event string, data map[string]any) {
		data["type"] = event
		data["sequence_number"] = seq
		seq++
		b, _ := json.Marshal(data)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		_ = rc.Flush()
	}
	send("response.created", map[string]any{"response": responseObject(id, req.Model, created, "in_progress", []any{}, nil)})
	for i, word := range words {
		if i > 0 {
			word = " " + word
		}
		send("response.output_text.delta", map[string]any{"item_id": msgID, "output_index": 0, "content_index": 0, "delta": word})
		select {
		case <-time.After(15 * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}
	send("response.output_text.done", map[string]any{"item_id": msgID, "output_index": 0, "content_index": 0, "text": text})
	send("response.completed", map[string]any{"response": responseObject(id, req.Model, created, "completed", []any{message}, use)})
}
