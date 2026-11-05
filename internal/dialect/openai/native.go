package openai

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/raven-clown/transponder/internal/schema"
)

// Peek is what the gateway reads from a request before forwarding it.
type Peek struct {
	Model        string
	Stream       bool
	IncludeUsage bool
}

// PeekRequest reads the routing fields of a chat or embeddings request.
func PeekRequest(body []byte) (Peek, error) {
	var w struct {
		Model         string `json:"model"`
		Stream        bool   `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return Peek{}, &RequestError{Code: "invalid_json", Message: "the request body is not valid JSON: " + err.Error()}
	}
	if w.Model == "" {
		return Peek{}, &RequestError{Param: "model", Code: "missing_field", Message: "model is required"}
	}
	p := Peek{Model: w.Model, Stream: w.Stream}
	if w.StreamOptions != nil {
		p.IncludeUsage = w.StreamOptions.IncludeUsage
	}
	return p, nil
}

// RewriteRequest sets model and, with forceUsage, stream_options.include_usage, keeping every other field.
func RewriteRequest(body []byte, model string, forceUsage bool) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return nil, errors.New("openai: request body is not a JSON object")
	}
	name, _ := json.Marshal(model)
	m["model"] = name
	if forceUsage {
		opts := map[string]json.RawMessage{}
		if raw, ok := m["stream_options"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if err := json.Unmarshal(raw, &opts); err != nil {
				return nil, errors.New("openai: stream_options is not a JSON object")
			}
		}
		opts["include_usage"] = json.RawMessage("true")
		raw, err := json.Marshal(opts)
		if err != nil {
			return nil, err
		}
		m["stream_options"] = raw
	}
	return json.Marshal(m)
}

type wireUsageIn struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func (u *wireUsageIn) canonical() schema.Usage {
	out := schema.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
		out.InputTokens -= out.CacheReadTokens
	}
	return out
}

// ParseUsage reads the usage object of a chat completion or embeddings response.
func ParseUsage(body []byte) (schema.Usage, bool) {
	var w struct {
		Usage *wireUsageIn `json:"usage"`
	}
	if err := json.Unmarshal(body, &w); err != nil || w.Usage == nil {
		return schema.Usage{}, false
	}
	return w.Usage.canonical(), true
}

// ParseChatRequest decodes a chat request already read into memory.
func ParseChatRequest(body []byte) (*schema.ChatRequest, error) {
	return DecodeChatRequest(bytes.NewReader(body))
}
