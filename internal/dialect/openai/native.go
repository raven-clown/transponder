package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"

	"github.com/raven-clown/transponder/internal/schema"
)

// Peek is what the gateway reads from a request before forwarding it.
type Peek struct {
	Model        string
	Stream       bool
	IncludeUsage bool
}

// PeekRequest reads the routing fields of a chat or embeddings request. Keys are matched exactly,
// as upstream servers match them, so the gateway and the provider always agree on model and stream.
func PeekRequest(body []byte) (Peek, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return Peek{}, invalidJSON("the request body is not a JSON object")
	}
	var p Peek
	if err := field(m, "model", &p.Model); err != nil {
		return Peek{}, err
	}
	if err := field(m, "stream", &p.Stream); err != nil {
		return Peek{}, err
	}
	if raw, ok := m["stream_options"]; ok && !isNull(raw) {
		var opts map[string]json.RawMessage
		if err := json.Unmarshal(raw, &opts); err != nil {
			return Peek{}, invalidJSON("stream_options must be an object")
		}
		if err := field(opts, "include_usage", &p.IncludeUsage); err != nil {
			return Peek{}, err
		}
	}
	if p.Model == "" {
		return Peek{}, &RequestError{Param: "model", Code: "missing_field", Message: "model is required"}
	}
	return p, nil
}

func field(m map[string]json.RawMessage, key string, dst any) error {
	raw, ok := m[key]
	if !ok || isNull(raw) {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return &RequestError{Param: key, Code: "invalid_json", Message: key + " has the wrong type"}
	}
	return nil
}

func isNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func invalidJSON(msg string) *RequestError {
	return &RequestError{Code: "invalid_json", Message: msg}
}

// RewriteRequest sets model and, with forceUsage, stream_options.include_usage, keeping every other field.
func RewriteRequest(body []byte, model string, forceUsage bool) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return nil, errors.New("openai: request body is not a JSON object")
	}
	for k := range m {
		for _, exact := range []string{"model", "stream", "stream_options"} {
			if k != exact && strings.EqualFold(k, exact) {
				delete(m, k)
			}
		}
	}
	name, _ := json.Marshal(model)
	m["model"] = name
	if forceUsage {
		opts := map[string]json.RawMessage{}
		if raw, ok := m["stream_options"]; ok && !isNull(raw) {
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
