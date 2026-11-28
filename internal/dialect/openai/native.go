package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/raven-clown/transponder/internal/schema"
)

// maxPresize bounds the size a rewritten request is given up front. A larger body grows the
// buffer as it is written. The gateway caps request bodies at 20 MiB, so this is never reached.
const maxPresize = 1 << 28

// Peek is what the gateway reads from a request before forwarding it.
type Peek struct {
	Model        string
	Stream       bool
	IncludeUsage bool
}

// PeekRequest reads the routing fields of a chat or embeddings request. Keys are matched exactly,
// as upstream servers match them, so the gateway and the provider always agree on model and stream.
func PeekRequest(body []byte) (Peek, error) {
	if !json.Valid(body) {
		return Peek{}, invalidJSON("the request body is not a JSON object")
	}
	var buf [32]member
	ms, ok := objectMembers(body, buf[:0])
	if !ok {
		return Peek{}, invalidJSON("the request body is not a JSON object")
	}
	var p Peek
	if err := field(ms, "model", &p.Model); err != nil {
		return Peek{}, err
	}
	if err := field(ms, "stream", &p.Stream); err != nil {
		return Peek{}, err
	}
	if m, ok := lastMember(ms, "stream_options"); ok && !isNull(m.value) {
		var obuf [8]member
		opts, ok := objectMembers(m.value, obuf[:0])
		if !ok {
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

// field reads the last member named key into dst, a *string or *bool, and leaves dst alone
// when the member is missing or null.
func field(ms []member, key string, dst any) error {
	m, ok := lastMember(ms, key)
	if !ok || isNull(m.value) {
		return nil
	}
	// The common shapes are read without encoding/json: true, false and a string of valid
	// UTF-8 without escapes, which encoding/json would return unchanged.
	switch d := dst.(type) {
	case *bool:
		switch string(m.value) {
		case "true":
			*d = true
			return nil
		case "false":
			*d = false
			return nil
		}
	case *string:
		if v := m.value; len(v) >= 2 && v[0] == '"' && bytes.IndexByte(v, '\\') < 0 && utf8.Valid(v) {
			*d = string(v[1 : len(v)-1])
			return nil
		}
	}
	if err := json.Unmarshal(m.value, dst); err != nil {
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
// Every other member is copied as the caller wrote it, so the body is neither decoded nor
// encoded again. model comes first, then stream_options when it is set, then the rest in order.
func RewriteRequest(body []byte, model string, forceUsage bool) ([]byte, error) {
	notObject := errors.New("openai: request body is not a JSON object")
	if !json.Valid(body) {
		return nil, notObject
	}
	var buf [32]member
	ms, ok := objectMembers(body, buf[:0])
	if !ok {
		return nil, notObject
	}
	if duplicateKeys(ms) {
		// encoding/json keeps the last of a repeated key; the map does the same, so the
		// provider gets each key once whatever its own parser would do with two.
		return rewriteByMap(body, model, forceUsage)
	}
	name, _ := json.Marshal(model)
	var capHint int
	if len(body) <= maxPresize && len(name) <= maxPresize {
		capHint = len(body) + len(name) + 48
	}
	out := make([]byte, 0, capHint)
	out = append(out, `{"model":`...)
	out = append(out, name...)
	if forceUsage {
		opts := []byte(`{"include_usage":true}`)
		if m, ok := lastMember(ms, "stream_options"); ok && !isNull(m.value) {
			var obuf [8]member
			om, ok := objectMembers(m.value, obuf[:0])
			if !ok {
				return nil, errors.New("openai: stream_options is not a JSON object")
			}
			if duplicateKeys(om) {
				return rewriteByMap(body, model, forceUsage)
			}
			opts = append([]byte(nil), `{"include_usage":true`...)
			for _, o := range om {
				if string(o.key) != "include_usage" {
					opts = append(opts, ',')
					opts = append(opts, m.value[o.start:o.end]...)
				}
			}
			opts = append(opts, '}')
		}
		out = append(out, `,"stream_options":`...)
		out = append(out, opts...)
	}
	for _, m := range ms {
		if rewritten(m.key, forceUsage) {
			continue
		}
		out = append(out, ',')
		out = append(out, body[m.start:m.end]...)
	}
	return append(out, '}'), nil
}

// rewritten reports whether RewriteRequest drops a member the caller sent: model, which it
// sets, stream_options when it sets that, and any spelling of model, stream or stream_options
// in other letter case, which a case-blind upstream could read in place of the real one.
func rewritten(key []byte, forceUsage bool) bool {
	switch string(key) {
	case "model":
		return true
	case "stream_options":
		return forceUsage
	case "stream":
		return false
	}
	for _, exact := range []string{"model", "stream", "stream_options"} {
		if bytes.EqualFold(key, []byte(exact)) {
			return true
		}
	}
	return false
}

// rewriteByMap is RewriteRequest for a body that repeats a key, where only the last copy is kept.
func rewriteByMap(body []byte, model string, forceUsage bool) ([]byte, error) {
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
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

// canonical splits cached input out of prompt tokens. OpenAI includes reasoning in
// completion tokens, so those are split out too. xAI reports reasoning apart: its
// total_tokens is prompt + completion + reasoning, and subtracting would drop them.
func (u *wireUsageIn) canonical() schema.Usage {
	out := schema.Usage{InputTokens: u.PromptTokens, OutputTokens: u.CompletionTokens}
	if u.PromptTokensDetails != nil {
		out.CacheReadTokens = u.PromptTokensDetails.CachedTokens
		out.InputTokens -= out.CacheReadTokens
	}
	if u.CompletionTokensDetails != nil {
		out.ReasoningTokens = u.CompletionTokensDetails.ReasoningTokens
		if !u.reasoningApart() {
			out.OutputTokens -= out.ReasoningTokens
		}
	}
	return out
}

// reasoningApart is true when reasoning tokens are not already inside completion tokens.
func (u *wireUsageIn) reasoningApart() bool {
	if u.CompletionTokensDetails == nil || u.TotalTokens == 0 {
		return false
	}
	r := u.CompletionTokensDetails.ReasoningTokens
	return r > 0 && u.TotalTokens == u.PromptTokens+u.CompletionTokens+r
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
