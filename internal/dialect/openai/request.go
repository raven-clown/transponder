package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/raven-clown/transponder/internal/schema"
)

// EncodeOptions adjust the request for one kind of server.
type EncodeOptions struct {
	// MaxCompletionTokens sends the limit as max_completion_tokens, which OpenAI's reasoning
	// models require, instead of max_tokens, which other compatible servers know.
	MaxCompletionTokens bool
}

// EncodeChatRequest renders a canonical request in the OpenAI wire format. A streamed request
// asks for usage at the end.
func EncodeChatRequest(req *schema.ChatRequest) ([]byte, error) {
	return EncodeChatRequestWith(req, EncodeOptions{})
}

// EncodeChatRequestWith is EncodeChatRequest with options.
func EncodeChatRequestWith(req *schema.ChatRequest, o EncodeOptions) ([]byte, error) {
	w := wireRequest{Model: req.Model, MaxTokens: req.MaxTokens, Temperature: req.Temperature, TopP: req.TopP, Stream: req.Stream}
	if o.MaxCompletionTokens {
		w.MaxTokens, w.MaxCompletionTokens = nil, req.MaxTokens
	}
	if req.Stream {
		w.StreamOptions = &wireStreamOptions{IncludeUsage: true}
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: m.Role, ToolCallID: m.ToolCallID}
		if len(m.ToolCalls) > 0 {
			wm.ToolCalls = encodeToolCalls(m.ToolCalls)
		}
		if m.Content == "" && len(m.ToolCalls) > 0 {
			wm.Content = json.RawMessage("null")
		} else {
			content, err := json.Marshal(m.Content)
			if err != nil {
				return nil, err
			}
			wm.Content = content
		}
		w.Messages = append(w.Messages, wm)
	}
	for _, t := range req.Tools {
		w.Tools = append(w.Tools, wireTool{Type: "function", Function: wireFunction{Name: t.Name, Description: t.Description, Parameters: t.Parameters}})
	}
	if len(req.Stop) > 0 {
		stop, err := json.Marshal(req.Stop)
		if err != nil {
			return nil, err
		}
		w.Stop = stop
	}
	if tc := req.ToolChoice; tc != nil {
		var v any = tc.Mode
		if tc.Mode == schema.ToolChoiceTool {
			v = struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}{Type: "function", Function: struct {
				Name string `json:"name"`
			}{tc.Name}}
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		w.ToolChoice = b
	}
	return json.Marshal(w)
}

type wireResponseIn struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []wireToolCall  `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsageIn `json:"usage"`
}

// DecodeChatResponse reads an OpenAI chat completion into canonical form, from its first choice.
func DecodeChatResponse(body []byte) (*schema.ChatResponse, error) {
	var w wireResponseIn
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("openai: decoding the response: %w", err)
	}
	if len(w.Choices) == 0 {
		return nil, errors.New("openai: the response has no choices")
	}
	ch := w.Choices[0]
	content, err := messageText(ch.Message.Content)
	if err != nil {
		return nil, err
	}
	resp := &schema.ChatResponse{ID: w.ID, Model: w.Model, FinishReason: ch.FinishReason,
		Message: schema.Message{Role: ch.Message.Role, Content: content}}
	if resp.Message.Role == "" {
		resp.Message.Role = schema.RoleAssistant
	}
	for _, tc := range ch.Message.ToolCalls {
		resp.Message.ToolCalls = append(resp.Message.ToolCalls, schema.ToolCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	if w.Usage != nil {
		resp.Usage = w.Usage.canonical()
	}
	return resp, nil
}

// ChunkDecoder turns the chunks of one OpenAI stream into canonical events.
type ChunkDecoder struct {
	started bool
}

type wireChunkIn struct {
	Choices []struct {
		Delta struct {
			Content   *string        `json:"content"`
			ToolCalls []wireToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsageIn `json:"usage"`
}

// Events returns the canonical events in one chunk.
func (d *ChunkDecoder) Events(data []byte) ([]schema.Event, error) {
	var c wireChunkIn
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("openai: decoding a stream chunk: %w", err)
	}
	var out []schema.Event
	if !d.started {
		d.started = true
		out = append(out, schema.Event{Type: schema.EventStart})
	}
	for _, ch := range c.Choices {
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			out = append(out, schema.Event{Type: schema.EventText, Text: *ch.Delta.Content})
		}
		for _, tc := range ch.Delta.ToolCalls {
			i := 0
			if tc.Index != nil {
				i = *tc.Index
			}
			if tc.ID != "" || tc.Function.Name != "" {
				out = append(out, schema.Event{Type: schema.EventToolStart, ToolIndex: i, ToolID: tc.ID, ToolName: tc.Function.Name})
			}
			if tc.Function.Arguments != "" {
				out = append(out, schema.Event{Type: schema.EventToolDelta, ToolIndex: i, ToolArgs: tc.Function.Arguments})
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			out = append(out, schema.Event{Type: schema.EventFinish, FinishReason: *ch.FinishReason})
		}
	}
	if c.Usage != nil {
		u := c.Usage.canonical()
		out = append(out, schema.Event{Type: schema.EventUsage, Usage: &u})
	}
	return out, nil
}

// EncodeEmbedRequest renders an embeddings request.
func EncodeEmbedRequest(req *schema.EmbedRequest) ([]byte, error) {
	return json.Marshal(struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}{req.Model, req.Input})
}

// DecodeEmbedResponse reads an embeddings response, putting the vectors in input order.
func DecodeEmbedResponse(body []byte) (*schema.EmbedResponse, error) {
	var w struct {
		Model string `json:"model"`
		Data  []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Usage *wireUsageIn `json:"usage"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("openai: decoding the embeddings: %w", err)
	}
	sort.SliceStable(w.Data, func(i, j int) bool { return w.Data[i].Index < w.Data[j].Index })
	resp := &schema.EmbedResponse{Model: w.Model}
	for _, d := range w.Data {
		resp.Vectors = append(resp.Vectors, d.Embedding)
	}
	if w.Usage != nil {
		resp.Usage = w.Usage.canonical()
	}
	return resp, nil
}
