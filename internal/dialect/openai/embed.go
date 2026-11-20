package openai

import (
	"bytes"
	"encoding/json"

	"github.com/raven-clown/transponder/internal/schema"
)

// ParseEmbedRequest decodes an embeddings request whose input is a string or a list of strings.
func ParseEmbedRequest(body []byte) (*schema.EmbedRequest, error) {
	var w struct {
		Model string          `json:"model"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, &RequestError{Code: "invalid_json", Message: "the request body is not valid JSON: " + err.Error()}
	}
	if w.Model == "" {
		return nil, &RequestError{Param: "model", Code: "missing_field", Message: "model is required"}
	}
	raw := bytes.TrimSpace(w.Input)
	missing := &RequestError{Param: "input", Code: "missing_field", Message: "input is required"}
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, missing
	}
	req := &schema.EmbedRequest{Model: w.Model}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		req.Input = []string{one}
		return req, nil
	}
	if err := json.Unmarshal(raw, &req.Input); err != nil {
		return nil, &RequestError{Param: "input", Code: "unsupported", Message: "input must be a string or a list of strings"}
	}
	if len(req.Input) == 0 {
		return nil, missing
	}
	return req, nil
}

type wireEmbedding struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type wireEmbedUsage struct {
	PromptTokens int64 `json:"prompt_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

// EncodeEmbedResponse renders vectors as an OpenAI embeddings response.
func EncodeEmbedResponse(resp *schema.EmbedResponse) ([]byte, error) {
	data := make([]wireEmbedding, len(resp.Vectors))
	for i, v := range resp.Vectors {
		data[i] = wireEmbedding{Object: "embedding", Index: i, Embedding: v}
	}
	tokens := resp.Usage.InputTokens + resp.Usage.CacheReadTokens
	return json.Marshal(struct {
		Object string          `json:"object"`
		Data   []wireEmbedding `json:"data"`
		Model  string          `json:"model"`
		Usage  wireEmbedUsage  `json:"usage"`
	}{"list", data, resp.Model, wireEmbedUsage{tokens, tokens}})
}
