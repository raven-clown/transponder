// Package gemini converts canonical calls to and from the Gemini generateContent API.
package gemini

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/tokens"
)

// skipSignature stands in for a thought signature on a function call that did not come from
// Gemini, such as one in a history started on another provider.
const skipSignature = "skip_thought_signature_validator"

type idParts struct {
	ID        string `json:"i,omitempty"`
	Signature string `json:"s,omitempty"`
}

// ToolCallID packs Gemini's function call id and thought signature into one tool call id, so
// the signature comes back with the tool result on the next turn. The id uses only letters,
// digits, _ and -, which every client accepts.
func ToolCallID(geminiID, signature string) string {
	if geminiID == "" {
		b := make([]byte, 8)
		_, _ = rand.Read(b)
		geminiID = "call_" + hex.EncodeToString(b)
	}
	b, _ := json.Marshal(idParts{ID: geminiID, Signature: signature})
	return "gm_" + base64.RawURLEncoding.EncodeToString(b)
}

func splitID(id string) (geminiID, signature string) {
	if rest, ok := strings.CutPrefix(id, "gm_"); ok {
		if raw, err := base64.RawURLEncoding.DecodeString(rest); err == nil {
			var p idParts
			if json.Unmarshal(raw, &p) == nil && p.ID != "" {
				return p.ID, p.Signature
			}
		}
	}
	return id, ""
}

type part struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
}

type functionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type functionResponse struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type declaration struct {
	Name                 string          `json:"name"`
	Description          string          `json:"description,omitempty"`
	ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema,omitempty"`
}

type tool struct {
	FunctionDeclarations []declaration `json:"functionDeclarations"`
}

type toolConfig struct {
	FunctionCallingConfig struct {
		Mode                 string   `json:"mode"`
		AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
	} `json:"functionCallingConfig"`
}

type generationConfig struct {
	MaxOutputTokens *int     `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
	StopSequences   []string `json:"stopSequences,omitempty"`
}

type request struct {
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
	Contents          []content         `json:"contents"`
	Tools             []tool            `json:"tools,omitempty"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

// EncodeRequest renders a canonical request as a generateContent body.
func EncodeRequest(req *schema.ChatRequest) ([]byte, error) {
	var w request
	names := map[string]string{}
	add := func(role string, p ...part) {
		if n := len(w.Contents); n > 0 && w.Contents[n-1].Role == role {
			w.Contents[n-1].Parts = append(w.Contents[n-1].Parts, p...)
			return
		}
		w.Contents = append(w.Contents, content{Role: role, Parts: p})
	}
	var system []string
	for _, m := range req.Messages {
		switch m.Role {
		case schema.RoleSystem:
			system = append(system, m.Content)
		case schema.RoleAssistant:
			var parts []part
			if m.Content != "" {
				parts = append(parts, part{Text: m.Content})
			}
			for _, c := range m.ToolCalls {
				gid, sig := splitID(c.ID)
				if sig == "" {
					sig = skipSignature
				}
				names[c.ID] = c.Name
				parts = append(parts, part{FunctionCall: &functionCall{ID: gid, Name: c.Name, Args: argsObject(c.Arguments)}, ThoughtSignature: sig})
			}
			if len(parts) > 0 {
				add("model", parts...)
			}
		case schema.RoleTool:
			gid, _ := splitID(m.ToolCallID)
			add("user", part{FunctionResponse: &functionResponse{ID: gid, Name: names[m.ToolCallID], Response: responseObject(m.Content)}})
		default:
			if m.Content != "" {
				add("user", part{Text: m.Content})
			}
		}
	}
	if len(system) > 0 {
		w.SystemInstruction = &content{Parts: []part{{Text: strings.Join(system, "\n\n")}}}
	}
	if len(req.Tools) > 0 {
		var decls []declaration
		for _, t := range req.Tools {
			decls = append(decls, declaration{Name: t.Name, Description: t.Description, ParametersJSONSchema: t.Parameters})
		}
		w.Tools = []tool{{FunctionDeclarations: decls}}
	}
	if tc := req.ToolChoice; tc != nil {
		w.ToolConfig = &toolConfig{}
		cfg := &w.ToolConfig.FunctionCallingConfig
		switch tc.Mode {
		case schema.ToolChoiceNone:
			cfg.Mode = "NONE"
		case schema.ToolChoiceRequired:
			cfg.Mode = "ANY"
		case schema.ToolChoiceTool:
			cfg.Mode, cfg.AllowedFunctionNames = "ANY", []string{tc.Name}
		default:
			cfg.Mode = "AUTO"
		}
	}
	if req.MaxTokens != nil || req.Temperature != nil || req.TopP != nil || len(req.Stop) > 0 {
		w.GenerationConfig = &generationConfig{MaxOutputTokens: req.MaxTokens, Temperature: req.Temperature, TopP: req.TopP, StopSequences: req.Stop}
	}
	return json.Marshal(w)
}

func argsObject(args string) json.RawMessage {
	var obj map[string]json.RawMessage
	var b bytes.Buffer
	if json.Unmarshal([]byte(args), &obj) != nil || obj == nil || json.Compact(&b, []byte(args)) != nil {
		return json.RawMessage(`{}`)
	}
	return b.Bytes()
}

// responseObject passes a JSON object result as is and wraps anything else, since Gemini wants
// an object.
func responseObject(text string) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(text), &obj) == nil && obj != nil {
		return argsObject(text)
	}
	b, _ := json.Marshal(map[string]string{"content": text})
	return b
}

type usageMetadata struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
}

// canonical keeps thinking apart as reasoning tokens, which are billed at the output price.
func (u usageMetadata) canonical() schema.Usage {
	return schema.Usage{
		InputTokens:     u.PromptTokenCount - u.CachedContentTokenCount,
		CacheReadTokens: u.CachedContentTokenCount,
		OutputTokens:    u.CandidatesTokenCount,
		ReasoningTokens: u.ThoughtsTokenCount,
	}
}

type response struct {
	ResponseID   string `json:"responseId"`
	ModelVersion string `json:"modelVersion"`
	Candidates   []struct {
		Content      content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback *struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	UsageMetadata *usageMetadata `json:"usageMetadata"`
}

func finishReason(reason string, calls bool) string {
	switch reason {
	case "MAX_TOKENS":
		return schema.FinishLength
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "LANGUAGE", "IMAGE_SAFETY":
		return schema.FinishContentFilter
	}
	if calls {
		return schema.FinishToolCalls
	}
	return schema.FinishStop
}

func toolCall(fc *functionCall, signature string) schema.ToolCall {
	return schema.ToolCall{ID: ToolCallID(fc.ID, signature), Name: fc.Name, Arguments: string(argsObject(string(fc.Args)))}
}

// DecodeResponse reads a generateContent response from its first candidate. Thought parts are
// skipped.
func DecodeResponse(body []byte) (*schema.ChatResponse, error) {
	var w response
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("gemini: decoding the response: %w", err)
	}
	resp := &schema.ChatResponse{ID: w.ResponseID, Model: w.ModelVersion, Message: schema.Message{Role: schema.RoleAssistant}}
	if w.UsageMetadata != nil {
		resp.Usage = w.UsageMetadata.canonical()
	}
	if len(w.Candidates) == 0 {
		resp.FinishReason = schema.FinishStop
		if w.PromptFeedback != nil && w.PromptFeedback.BlockReason != "" {
			resp.FinishReason = schema.FinishContentFilter
		}
		return resp, nil
	}
	c := w.Candidates[0]
	var text strings.Builder
	for _, p := range c.Content.Parts {
		switch {
		case p.Thought:
		case p.FunctionCall != nil:
			resp.Message.ToolCalls = append(resp.Message.ToolCalls, toolCall(p.FunctionCall, p.ThoughtSignature))
		default:
			text.WriteString(p.Text)
		}
	}
	resp.Message.Content = text.String()
	resp.FinishReason = finishReason(c.FinishReason, len(resp.Message.ToolCalls) > 0)
	return resp, nil
}

// StreamDecoder turns the chunks of a streamGenerateContent response into canonical events.
type StreamDecoder struct {
	started, done bool
	calls         int
	usage         *usageMetadata
}

// Events returns the canonical events in one chunk.
func (d *StreamDecoder) Events(data []byte) ([]schema.Event, error) {
	var w response
	if err := json.Unmarshal(data, &w); err != nil {
		return nil, fmt.Errorf("gemini: decoding a stream chunk: %w", err)
	}
	var out []schema.Event
	if !d.started {
		d.started = true
		out = append(out, schema.Event{Type: schema.EventStart})
	}
	if w.UsageMetadata != nil {
		d.usage = w.UsageMetadata
	}
	if len(w.Candidates) == 0 && w.PromptFeedback != nil && w.PromptFeedback.BlockReason != "" && !d.done {
		return append(out, d.finish(schema.FinishContentFilter)...), nil
	}
	for _, c := range w.Candidates {
		for _, p := range c.Content.Parts {
			switch {
			case p.Thought:
			case p.FunctionCall != nil:
				tc := toolCall(p.FunctionCall, p.ThoughtSignature)
				out = append(out,
					schema.Event{Type: schema.EventToolStart, ToolIndex: d.calls, ToolID: tc.ID, ToolName: tc.Name},
					schema.Event{Type: schema.EventToolDelta, ToolIndex: d.calls, ToolArgs: tc.Arguments})
				d.calls++
			case p.Text != "":
				out = append(out, schema.Event{Type: schema.EventText, Text: p.Text})
			}
		}
		if c.FinishReason != "" && !d.done {
			out = append(out, d.finish(finishReason(c.FinishReason, d.calls > 0))...)
		}
	}
	return out, nil
}

func (d *StreamDecoder) finish(reason string) []schema.Event {
	d.done = true
	var u schema.Usage
	if d.usage != nil {
		u = d.usage.canonical()
	}
	return []schema.Event{{Type: schema.EventFinish, FinishReason: reason}, {Type: schema.EventUsage, Usage: &u}}
}

// Done reports whether a finish reason arrived.
func (d *StreamDecoder) Done() bool { return d.done }

// ErrModelName means a model name has characters no Gemini model name has. The name becomes
// part of the URL path, so anything else could reach another endpoint with the gateway's key.
var ErrModelName = errors.New("gemini: model names may only have letters, digits, '.', '_' and '-'")

// ModelPath returns the models/{model} resource name, after checking the name.
func ModelPath(model string) (string, error) {
	name := strings.TrimPrefix(model, "models/")
	if name == "" {
		return "", ErrModelName
	}
	for _, r := range name {
		ok := r == '.' || r == '_' || r == '-' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return "", ErrModelName
		}
	}
	if strings.Contains(name, "..") {
		return "", ErrModelName
	}
	return "models/" + name, nil
}

// EncodeEmbedRequest renders a batchEmbedContents body.
func EncodeEmbedRequest(model string, input []string) ([]byte, error) {
	path, err := ModelPath(model)
	if err != nil {
		return nil, err
	}
	type embedReq struct {
		Model   string  `json:"model"`
		Content content `json:"content"`
	}
	reqs := make([]embedReq, len(input))
	for i, in := range input {
		reqs[i] = embedReq{Model: path, Content: content{Parts: []part{{Text: in}}}}
	}
	return json.Marshal(struct {
		Requests []embedReq `json:"requests"`
	}{reqs})
}

// DecodeEmbedResponse reads a batchEmbedContents response. Gemini does not always report
// tokens for embeddings, so they are estimated from the input at four characters a token.
func DecodeEmbedResponse(body []byte, model string, input []string) (*schema.EmbedResponse, error) {
	var w struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.Unmarshal(body, &w); err != nil {
		return nil, fmt.Errorf("gemini: decoding the embeddings: %w", err)
	}
	resp := &schema.EmbedResponse{Model: model}
	for _, e := range w.Embeddings {
		resp.Vectors = append(resp.Vectors, e.Values)
	}
	for _, in := range input {
		resp.Usage.InputTokens += tokens.Estimate(in)
	}
	return resp, nil
}
