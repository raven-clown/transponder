package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/tokens"
)

var zeroUsage schema.Usage

func (g *gateway) chat(w http.ResponseWriter, r *http.Request) {
	c := g.begin(w, r, "chat.completions")
	if c == nil {
		return
	}
	defer c.done()
	peek, err := openai.PeekRequest(c.body)
	if err != nil {
		writeRequestError(w, err)
		return
	}
	if !g.resolve(c, peek.Model) {
		return
	}
	if !g.admit(c, func() limits.Estimate {
		top := topFields(c.body)
		return limits.Estimate{InputTokens: estimateInput(c.body), OutputTokens: maxField(top, "max_completion_tokens", "max_tokens"), Choices: maxField(top, "n")}
	}) {
		return
	}
	out, _ := c.ticket.Caps()
	orig := c.body
	g.run(c, func() error {
		// The cap is written per target, since OpenAI and its look-alikes name the field differently.
		c.body = orig
		if out > 0 {
			c.body = capOpenAI(orig, c.resolved.Type, out)
		}
		n, native := nativeTarget(c, "openai")
		switch {
		case peek.Stream && native:
			return g.chatStreamNative(c, n, peek.IncludeUsage)
		case peek.Stream:
			return g.chatStreamCanonical(c, peek.IncludeUsage)
		case native:
			return g.chatNative(c, n)
		default:
			return g.chatCanonical(c)
		}
	})
}

func (g *gateway) chatNative(c *call, n provider.Native) error {
	body, err := openai.RewriteRequest(c.body, c.resolved.Model, false)
	if err != nil {
		return badInput(&openai.RequestError{Code: "invalid_json", Message: err.Error()})
	}
	body = openai.ShortenToolCallIDs(body)
	resp, err := n.Native(c.r.Context(), &provider.NativeCall{Path: "/chat/completions", Body: body, Header: c.r.Header})
	if err != nil {
		return failed(err, true)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := readUpstream(resp)
	if err != nil {
		return failed(err, true)
	}
	u, ok := openai.ParseUsage(out)
	if !ok {
		u = schema.Usage{InputTokens: promptTokens(c.body), OutputTokens: estimateTokens(replyText(out))}
		c.rec.Estimated = true
	}
	cost := g.price(c.r.Context(), c, u)
	c.rec.Status = resp.StatusCode
	g.recordCall(c.r.Context(), c)

	setUsageHeaders(c.w.Header(), u, cost)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(resp.StatusCode)
	_, _ = c.w.Write(out)
	return nil
}

func (g *gateway) chatCanonical(c *call) error {
	req, err := openai.ParseChatRequest(c.body)
	if err != nil {
		return badInput(err)
	}
	req.Model = c.resolved.Model
	resp, err := c.resolved.Provider.Chat(upstreamContext(c), req)
	if err != nil {
		return failed(err, false)
	}
	body, err := openai.EncodeChatResponse(resp, c.rec.Time)
	if err != nil {
		writeError(c.w, http.StatusInternalServerError, "api_error", "", "", "could not encode the response")
		return nil //nolint:nilerr // the 500 above is the answer
	}
	u := resp.Usage
	if !reported(u) {
		u = schema.Usage{InputTokens: promptTokens(c.body), OutputTokens: estimateTokens(answerText(resp))}
		c.rec.Estimated = true
	}
	cost := g.price(c.r.Context(), c, u)
	c.rec.Status = http.StatusOK
	g.recordCall(c.r.Context(), c)

	setUsageHeaders(c.w.Header(), u, cost)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(body) //nolint:gosec // JSON we encoded, served as application/json
	return nil
}

func writeRequestError(w http.ResponseWriter, err error) {
	var bad *openai.RequestError
	if errors.As(err, &bad) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", bad.Code, bad.Param, bad.Message)
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request_error", "", "", err.Error())
}

// replyText joins the message contents of a chat completion, for estimating output tokens.
func replyText(body []byte) string {
	var w struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &w) != nil {
		return string(body)
	}
	var b strings.Builder
	for _, ch := range w.Choices {
		b.WriteString(ch.Message.Content)
	}
	return b.String()
}

// reported tells usage a provider sent from none at all, which translated responses and
// streams carry as zeros. No real call uses zero tokens.
func reported(u schema.Usage) bool {
	return u != schema.Usage{}
}

// answerText joins the text and tool arguments of a translated answer, for estimating output tokens.
func answerText(resp *schema.ChatResponse) string {
	var b strings.Builder
	b.WriteString(resp.Message.Text())
	for _, tc := range resp.Message.ToolCalls {
		b.WriteString(tc.Name)
		b.WriteString(tc.Arguments)
	}
	return b.String()
}

// promptTokens estimates the input of a chat request from the text of its messages, plus
// tokens.Image for each image, for a provider that reports no usage.
func promptTokens(body []byte) int64 {
	var w struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &w) != nil {
		return estimateTokens(string(body))
	}
	var b strings.Builder
	var images int64
	for _, m := range w.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			b.WriteString(s)
			continue
		}
		var parts []json.RawMessage
		if json.Unmarshal(m.Content, &parts) != nil {
			continue
		}
		for _, raw := range parts {
			var p struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			_ = json.Unmarshal(raw, &p)
			switch p.Type {
			case "text":
				b.Write([]byte(p.Text))
			case "image_url":
				images++
			default:
				// A file or audio part is read by the model too, so its data counts.
				b.Write(raw)
			}
		}
	}
	return estimateTokens(b.String()) + images*tokens.Image
}
