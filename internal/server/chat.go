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
	if !g.admit(c, limits.Estimate{InputTokens: estimateInput(c.body), OutputTokens: requestedMax(c.body, "max_completion_tokens", "max_tokens"), Choices: requestedMax(c.body, "n")}) {
		return
	}
	if out, _ := c.ticket.Caps(); out > 0 {
		c.body = capOpenAI(c.body, c.resolved.Type, out)
	}
	n, native := nativeTarget(c, "openai")
	switch {
	case peek.Stream && native:
		g.chatStreamNative(c, n, peek.IncludeUsage)
	case peek.Stream:
		g.chatStreamCanonical(c, peek.IncludeUsage)
	case native:
		g.chatNative(c, n)
	default:
		g.chatCanonical(c)
	}
}

func (g *gateway) chatNative(c *call, n provider.Native) {
	body, err := openai.RewriteRequest(c.body, c.resolved.Model, false)
	if err != nil {
		writeRequestError(c.w, &openai.RequestError{Code: "invalid_json", Message: err.Error()})
		return
	}
	resp, err := n.Native(c.r.Context(), &provider.NativeCall{Path: "/chat/completions", Body: body, Header: c.r.Header})
	if err != nil {
		g.upstreamFailed(c, err, true)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := readUpstream(resp)
	if err != nil {
		g.upstreamFailed(c, err, true)
		return
	}
	u, ok := openai.ParseUsage(out)
	if !ok {
		u = schema.Usage{InputTokens: estimateTokens(promptText(c.body)), OutputTokens: estimateTokens(replyText(out))}
		c.rec.Estimated = true
	}
	cost := g.price(c.r.Context(), c, u)
	c.rec.Status = resp.StatusCode
	g.recordCall(c.r.Context(), c)

	setUsageHeaders(c.w.Header(), u, cost)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(resp.StatusCode)
	_, _ = c.w.Write(out)
}

func (g *gateway) chatCanonical(c *call) {
	req, err := openai.ParseChatRequest(c.body)
	if err != nil {
		writeRequestError(c.w, err)
		return
	}
	req.Model = c.resolved.Model
	resp, err := c.resolved.Provider.Chat(upstreamContext(c), req)
	if err != nil {
		g.upstreamFailed(c, err, false)
		return
	}
	body, err := openai.EncodeChatResponse(resp, c.rec.Time)
	if err != nil {
		writeError(c.w, http.StatusInternalServerError, "api_error", "", "", "could not encode the response")
		return
	}
	cost := g.price(c.r.Context(), c, resp.Usage)
	c.rec.Status = http.StatusOK
	g.recordCall(c.r.Context(), c)

	setUsageHeaders(c.w.Header(), resp.Usage, cost)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(body) //nolint:gosec // JSON we encoded, served as application/json
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

// promptText joins the text of the request messages, for estimating input tokens.
func promptText(body []byte) string {
	var w struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &w) != nil {
		return string(body)
	}
	var b strings.Builder
	for _, m := range w.Messages {
		var s string
		if json.Unmarshal(m.Content, &s) == nil {
			b.WriteString(s)
			continue
		}
		var parts []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &parts) == nil {
			for _, p := range parts {
				b.WriteString(p.Text)
			}
		}
	}
	return b.String()
}
