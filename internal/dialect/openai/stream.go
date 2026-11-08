package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

// DoneFrame ends an OpenAI stream.
const DoneFrame = "data: [DONE]\n\n"

var (
	// ErrNoDone means the stream ended cleanly but before [DONE], so the answer may be cut short.
	ErrNoDone = errors.New("openai: stream ended before [DONE]")
	// ErrEventTooLarge means one line or event is over the size limit.
	ErrEventTooLarge = sse.ErrEventTooLarge
)

// SSEReader reads the data payloads of an OpenAI stream.
type SSEReader struct {
	r    *sse.Reader
	done bool
}

// NewSSEReader reads events from r.
func NewSSEReader(r io.Reader) *SSEReader {
	return &SSEReader{r: sse.NewReader(r)}
}

// Next returns the next data payload. It returns io.EOF after [DONE], ErrNoDone on a clean
// end without it, and io.ErrUnexpectedEOF when the stream stops inside an event.
func (s *SSEReader) Next() ([]byte, error) {
	if s.done {
		return nil, io.EOF
	}
	ev, err := s.r.Next()
	if bytes.Equal(ev.Data, []byte("[DONE]")) && (err == nil || errors.Is(err, io.ErrUnexpectedEOF)) {
		s.done = true
		return nil, io.EOF
	}
	switch {
	case errors.Is(err, io.EOF):
		return nil, ErrNoDone
	case err != nil:
		return nil, err
	}
	return ev.Data, nil
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
	Role      string         `json:"role,omitempty"`
	Content   *string        `json:"content,omitempty"`
	ToolCalls []wireToolCall `json:"tool_calls,omitempty"`
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
	case schema.EventToolStart:
		i := ev.ToolIndex
		c.Choices = append(c.Choices, wireChunkChoice{Delta: wireDelta{ToolCalls: []wireToolCall{{
			Index: &i, ID: ev.ToolID, Type: "function", Function: wireFunctionCall{Name: ev.ToolName},
		}}}})
	case schema.EventToolDelta:
		i := ev.ToolIndex
		c.Choices = append(c.Choices, wireChunkChoice{Delta: wireDelta{ToolCalls: []wireToolCall{{
			Index: &i, Function: wireFunctionCall{Arguments: ev.ToolArgs},
		}}}})
	case schema.EventFinish:
		reason := ev.FinishReason
		c.Choices = append(c.Choices, wireChunkChoice{FinishReason: &reason})
	case schema.EventUsage:
		if ev.Usage == nil {
			return nil, nil
		}
		c.Usage = encodeUsage(*ev.Usage)
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
	out := append([]byte("data: "), payload...)
	return append(out, '\n', '\n')
}
