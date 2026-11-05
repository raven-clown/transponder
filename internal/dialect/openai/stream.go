package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/raven-clown/transponder/internal/schema"
)

// DoneFrame ends an OpenAI stream.
const DoneFrame = "data: [DONE]\n\n"

// SSEReader reads the data payloads of a server-sent event stream.
type SSEReader struct {
	r    *bufio.Reader
	done bool
}

// NewSSEReader reads events from r.
func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{r: bufio.NewReader(r)}
}

// Next returns the next data payload. It returns io.EOF after [DONE] or a clean end,
// and io.ErrUnexpectedEOF when the stream stops inside an event.
func (s *SSEReader) Next() ([]byte, error) {
	if s.done {
		return nil, io.EOF
	}
	var data []byte
	for {
		line, err := s.r.ReadBytes('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				if len(line) > 0 || data != nil {
					return nil, io.ErrUnexpectedEOF
				}
				return nil, io.EOF
			}
			return nil, err
		}
		line = bytes.TrimRight(line, "\r\n")
		switch {
		case len(line) == 0:
			if data != nil {
				return data, nil
			}
		case bytes.HasPrefix(line, []byte("data:")):
			payload := bytes.TrimPrefix(bytes.TrimPrefix(line, []byte("data:")), []byte(" "))
			if bytes.Equal(payload, []byte("[DONE]")) {
				s.done = true
				return nil, io.EOF
			}
			if data != nil {
				data = append(data, '\n')
			}
			data = append(data, payload...)
		}
	}
}

// ChunkUsage reads the usage of a stream chunk. usageOnly is true for the chunk with no choices
// that include_usage adds at the end.
func ChunkUsage(data []byte) (u schema.Usage, ok bool, usageOnly bool) {
	var w struct {
		Choices []json.RawMessage `json:"choices"`
		Usage   *wireUsageIn      `json:"usage"`
	}
	if err := json.Unmarshal(data, &w); err != nil || w.Usage == nil {
		return schema.Usage{}, false, false
	}
	return w.Usage.canonical(), true, len(w.Choices) == 0
}

type wireDelta struct {
	Role    string  `json:"role,omitempty"`
	Content *string `json:"content,omitempty"`
}

type wireChunkChoice struct {
	Index        int       `json:"index"`
	Delta        wireDelta `json:"delta"`
	FinishReason *string   `json:"finish_reason"`
}

type wireChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []wireChunkChoice `json:"choices"`
	Usage   *wireUsage        `json:"usage,omitempty"`
}

// EncodeChunk renders one canonical event as an SSE frame. It returns nil for events OpenAI has no chunk for.
func EncodeChunk(id, model string, created time.Time, ev schema.Event) ([]byte, error) {
	c := wireChunk{ID: id, Object: "chat.completion.chunk", Created: created.Unix(), Model: model, Choices: []wireChunkChoice{}}
	switch ev.Type {
	case schema.EventStart:
		empty := ""
		c.Choices = append(c.Choices, wireChunkChoice{Delta: wireDelta{Role: "assistant", Content: &empty}})
	case schema.EventText:
		text := ev.Text
		c.Choices = append(c.Choices, wireChunkChoice{Delta: wireDelta{Content: &text}})
	case schema.EventFinish:
		reason := ev.FinishReason
		c.Choices = append(c.Choices, wireChunkChoice{FinishReason: &reason})
	case schema.EventUsage:
		if ev.Usage == nil {
			return nil, nil
		}
		c.Usage = &wireUsage{
			PromptTokens:     ev.Usage.InputTokens + ev.Usage.CacheReadTokens,
			CompletionTokens: ev.Usage.OutputTokens,
			TotalTokens:      ev.Usage.InputTokens + ev.Usage.CacheReadTokens + ev.Usage.OutputTokens,
		}
	default:
		return nil, nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return frame(b), nil
}

// EncodeStreamError renders an error event for a stream that cannot continue.
func EncodeStreamError(code, message string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "api_error", "code": code}})
	return frame(b)
}

func frame(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+8)
	out = append(out, "data: "...)
	out = append(out, payload...)
	return append(out, '\n', '\n')
}
