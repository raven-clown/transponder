// Package echo is a provider that answers without calling anyone, for tests and demos.
package echo

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"unicode/utf8"

	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
)

// Echo repeats the last user message back.
type Echo struct {
	name string
}

var _ provider.Provider = (*Echo)(nil)

// New returns an echo provider called name.
func New(name string) *Echo {
	return &Echo{name: name}
}

// Name returns the configured name.
func (e *Echo) Name() string { return e.name }

// Dialect returns "none": echo has no native wire format.
func (e *Echo) Dialect() string { return "none" }

// Chat replies with "echo: " and the last user message.
func (e *Echo) Chat(_ context.Context, req *schema.ChatRequest) (*schema.ChatResponse, error) {
	var input int64
	last := ""
	for _, m := range req.Messages {
		input += tokens(m.Content)
		if m.Role == "user" {
			last = m.Content
		}
	}
	reply := "echo: " + last
	id := make([]byte, 8)
	_, _ = rand.Read(id)
	return &schema.ChatResponse{
		ID:           "echo-" + hex.EncodeToString(id),
		Model:        req.Model,
		Message:      schema.Message{Role: "assistant", Content: reply},
		FinishReason: "stop",
		Usage:        schema.Usage{InputTokens: input, OutputTokens: tokens(reply)},
	}, nil
}

// ChatStream sends the Chat reply one word at a time.
func (e *Echo) ChatStream(ctx context.Context, req *schema.ChatRequest) (schema.Stream, error) {
	resp, err := e.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	events := []schema.Event{{Type: schema.EventStart}}
	for _, w := range words(resp.Message.Content) {
		events = append(events, schema.Event{Type: schema.EventText, Text: w})
	}
	u := resp.Usage
	events = append(events,
		schema.Event{Type: schema.EventFinish, FinishReason: resp.FinishReason},
		schema.Event{Type: schema.EventUsage, Usage: &u})
	return &stream{events: events}, nil
}

// words splits s after each run of spaces, so joining the parts gives s back.
func words(s string) []string {
	var out []string
	start := 0
	for i := 1; i < len(s); i++ {
		if s[i-1] == ' ' && s[i] != ' ' {
			out = append(out, s[start:i])
			start = i
		}
	}
	return append(out, s[start:])
}

type stream struct {
	events []schema.Event
}

func (s *stream) Recv() (schema.Event, error) {
	if len(s.events) == 0 {
		return schema.Event{}, io.EOF
	}
	ev := s.events[0]
	s.events = s.events[1:]
	return ev, nil
}

func (s *stream) Close() error { return nil }

// Embed returns an 8-dimension vector per input, taken from its SHA-256.
func (e *Echo) Embed(_ context.Context, req *schema.EmbedRequest) (*schema.EmbedResponse, error) {
	resp := &schema.EmbedResponse{Model: req.Model, Vectors: make([][]float32, len(req.Input))}
	for i, in := range req.Input {
		sum := sha256.Sum256([]byte(in))
		v := make([]float32, 8)
		for j := range v {
			v[j] = float32(sum[j]) / 255
		}
		resp.Vectors[i] = v
		resp.Usage.InputTokens += tokens(in)
	}
	return resp, nil
}

func tokens(s string) int64 {
	return int64((utf8.RuneCountInString(s) + 3) / 4)
}
