package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

// StreamEncoder turns canonical events into Anthropic stream frames. Text and each tool call
// get their own content block.
type StreamEncoder struct {
	id, model string
	started   bool
	open      int // index of the open block, -1 when none
	next      int
	toolBlock map[int]int
	finish    string
	usage     schema.Usage
}

// NewStreamEncoder returns an encoder for one message.
func NewStreamEncoder(id, model string) *StreamEncoder {
	return &StreamEncoder{id: id, model: model, open: -1, toolBlock: map[int]int{}}
}

func frame(name string, v any) []byte {
	b, _ := json.Marshal(v)
	return sse.Frame(name, b)
}

type blockStart struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock any    `json:"content_block"`
}

type blockDelta struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	Delta any    `json:"delta"`
}

type textDelta struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type jsonDelta struct {
	Type        string `json:"type"`
	PartialJSON string `json:"partial_json"`
}

type blockStop struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
}

func (e *StreamEncoder) start() [][]byte {
	if e.started {
		return nil
	}
	e.started = true
	type startUsage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	}
	type startMessage struct {
		ID           string      `json:"id"`
		Type         string      `json:"type"`
		Role         string      `json:"role"`
		Model        string      `json:"model"`
		Content      []wireBlock `json:"content"`
		StopReason   *string     `json:"stop_reason"`
		StopSequence *string     `json:"stop_sequence"`
		Usage        startUsage  `json:"usage"`
	}
	return [][]byte{frame("message_start", struct {
		Type    string       `json:"type"`
		Message startMessage `json:"message"`
	}{"message_start", startMessage{ID: e.id, Type: "message", Role: "assistant", Model: e.model, Content: []wireBlock{}}})}
}

func (e *StreamEncoder) closeBlock() [][]byte {
	if e.open < 0 {
		return nil
	}
	i := e.open
	e.open = -1
	return [][]byte{frame("content_block_stop", blockStop{"content_block_stop", i})}
}

func (e *StreamEncoder) openBlock(block any) [][]byte {
	out := e.closeBlock()
	e.open = e.next
	e.next++
	return append(out, frame("content_block_start", blockStart{"content_block_start", e.open, block}))
}

// Frames returns the frames for one event.
func (e *StreamEncoder) Frames(ev schema.Event) [][]byte {
	out := e.start()
	switch ev.Type {
	case schema.EventText:
		if ev.Text == "" {
			break
		}
		if _, isTool := e.toolAt(e.open); e.open < 0 || isTool {
			out = append(out, e.openBlock(textDelta{"text", ""})...)
		}
		out = append(out, frame("content_block_delta", blockDelta{"content_block_delta", e.open, textDelta{"text_delta", ev.Text}}))
	case schema.EventToolStart:
		out = append(out, e.openBlock(struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}{"tool_use", ev.ToolID, ev.ToolName, json.RawMessage(`{}`)})...)
		e.toolBlock[ev.ToolIndex] = e.open
	case schema.EventToolDelta:
		if i, ok := e.toolBlock[ev.ToolIndex]; ok && ev.ToolArgs != "" {
			out = append(out, frame("content_block_delta", blockDelta{"content_block_delta", i, jsonDelta{"input_json_delta", ev.ToolArgs}}))
		}
	case schema.EventFinish:
		out = append(out, e.closeBlock()...)
		e.finish = ev.FinishReason
	case schema.EventUsage:
		if ev.Usage != nil {
			e.usage = *ev.Usage
		}
	}
	return out
}

func (e *StreamEncoder) toolAt(block int) (int, bool) {
	for t, b := range e.toolBlock {
		if b == block {
			return t, true
		}
	}
	return 0, false
}

type deltaUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens,omitempty"`
}

// End returns the frames that close the message: the stop reason, the usage and message_stop.
func (e *StreamEncoder) End() [][]byte {
	out := append(e.start(), e.closeBlock()...)
	out = append(out, frame("message_delta", struct {
		Type  string `json:"type"`
		Delta struct {
			StopReason   string  `json:"stop_reason"`
			StopSequence *string `json:"stop_sequence"`
		} `json:"delta"`
		Usage deltaUsage `json:"usage"`
	}{Type: "message_delta", Delta: struct {
		StopReason   string  `json:"stop_reason"`
		StopSequence *string `json:"stop_sequence"`
	}{StopReason: stopReason(e.finish)}, Usage: deltaUsage{
		InputTokens: e.usage.InputTokens, OutputTokens: e.usage.OutputTokens,
		CacheCreationInputTokens: e.usage.CacheWriteTokens, CacheReadInputTokens: e.usage.CacheReadTokens,
	}}))
	return append(out, frame("message_stop", struct {
		Type string `json:"type"`
	}{"message_stop"}))
}

// ErrorFrame is the event that ends a stream that cannot go on.
func ErrorFrame(errType, message string) []byte {
	return frame("error", wireError{Type: "error", Error: wireErrorDetail{Type: errType, Message: message}})
}

// StreamError is an error event sent by Anthropic in the middle of a stream.
type StreamError struct {
	Type, Message string
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("anthropic stream error %s: %s", e.Type, e.Message)
}

// StreamDecoder turns the events of one Anthropic stream into canonical events, and keeps the
// usage it has seen so far.
type StreamDecoder struct {
	usage  schema.Usage
	done   bool
	tools  int
	toolOf map[int]int
}

type wireEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage *wireUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *wireBlock `json:"content_block"`
	Delta        *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		InputTokens              *int64 `json:"input_tokens"`
		OutputTokens             *int64 `json:"output_tokens"`
		CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
	Error *wireErrorDetail `json:"error"`
}

// Events returns the canonical events for one stream event.
func (d *StreamDecoder) Events(ev sse.Event) ([]schema.Event, error) {
	var w wireEvent
	if err := json.Unmarshal(ev.Data, &w); err != nil {
		return nil, fmt.Errorf("anthropic: decoding a stream event: %w", err)
	}
	if d.toolOf == nil {
		d.toolOf = map[int]int{}
	}
	switch w.Type {
	case "message_start":
		if w.Message != nil && w.Message.Usage != nil {
			d.usage = w.Message.Usage.canonical()
		}
		return []schema.Event{{Type: schema.EventStart}}, nil
	case "content_block_start":
		if w.ContentBlock != nil && w.ContentBlock.Type == "tool_use" {
			t := d.tools
			d.tools++
			d.toolOf[w.Index] = t
			return []schema.Event{{Type: schema.EventToolStart, ToolIndex: t, ToolID: w.ContentBlock.ID, ToolName: w.ContentBlock.Name}}, nil
		}
	case "content_block_delta":
		if w.Delta == nil {
			break
		}
		switch w.Delta.Type {
		case "text_delta":
			return []schema.Event{{Type: schema.EventText, Text: w.Delta.Text}}, nil
		case "input_json_delta":
			if t, ok := d.toolOf[w.Index]; ok && w.Delta.PartialJSON != "" {
				return []schema.Event{{Type: schema.EventToolDelta, ToolIndex: t, ToolArgs: w.Delta.PartialJSON}}, nil
			}
		}
	case "message_delta":
		d.addUsage(w)
		if w.Delta != nil && w.Delta.StopReason != "" {
			return []schema.Event{{Type: schema.EventFinish, FinishReason: finishReason(w.Delta.StopReason)}}, nil
		}
	case "message_stop":
		d.done = true
		u := d.usage
		return []schema.Event{{Type: schema.EventUsage, Usage: &u}}, nil
	case "error":
		se := &StreamError{Type: "api_error"}
		if w.Error != nil {
			se.Type, se.Message = w.Error.Type, w.Error.Message
		}
		return nil, se
	}
	return nil, nil
}

func (d *StreamDecoder) addUsage(w wireEvent) {
	if w.Usage == nil {
		return
	}
	set := func(dst *int64, v *int64) {
		if v != nil {
			*dst = *v
		}
	}
	set(&d.usage.OutputTokens, w.Usage.OutputTokens)
	set(&d.usage.InputTokens, w.Usage.InputTokens)
	set(&d.usage.CacheWriteTokens, w.Usage.CacheCreationInputTokens)
	set(&d.usage.CacheReadTokens, w.Usage.CacheReadInputTokens)
}

// Done reports whether message_stop arrived.
func (d *StreamDecoder) Done() bool { return d.done }

// Usage returns the usage seen so far.
func (d *StreamDecoder) Usage() schema.Usage { return d.usage }
