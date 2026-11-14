package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/raven-clown/transponder/internal/dialect/anthropic"
	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

const streamCut = "the provider stopped in the middle of the stream"

// messages serves the Anthropic Messages API. Anthropic targets get the body as sent, with
// only the model changed; others get a translation.
func (g *gateway) messages(w http.ResponseWriter, r *http.Request) {
	c := g.beginWith(w, r, "messages", writeAnthropicError)
	if c == nil {
		return
	}
	defer c.done()
	peek, err := anthropic.PeekRequest(c.body)
	if err != nil {
		writeAnthropicRequestError(w, err)
		return
	}
	if !g.resolve(c, peek.Model) {
		return
	}
	if !g.admit(c, limits.Estimate{InputTokens: estimateInput(c.body), OutputTokens: requestedMax(c.body, "max_tokens")}) {
		return
	}
	if out, reasoning := c.ticket.Caps(); out > 0 || reasoning > 0 {
		c.body = capAnthropic(c.body, out, reasoning)
	}
	g.run(c, func() error {
		n, native := nativeTarget(c, "anthropic")
		switch {
		case peek.Stream && native:
			return g.messagesStreamNative(c, n)
		case peek.Stream:
			return g.messagesStreamTranslated(c)
		case native:
			return g.messagesNative(c, n)
		default:
			return g.messagesTranslated(c)
		}
	})
}

func writeAnthropicRequestError(w http.ResponseWriter, err error) {
	writeAnthropicError(w, http.StatusBadRequest, "", "", "", err.Error())
}

func (g *gateway) messagesNative(c *call, n provider.Native) error {
	body, err := openai.RewriteRequest(c.body, c.resolved.Model, false)
	if err != nil {
		return badInput(err)
	}
	resp, err := n.Native(c.r.Context(), &provider.NativeCall{Path: "/messages", Body: body, Header: c.r.Header})
	if err != nil {
		return failed(err, true)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := readUpstream(resp)
	if err != nil {
		return failed(err, true)
	}
	u, ok := anthropic.ParseUsage(out)
	if !ok {
		u = schema.Usage{InputTokens: estimateTokens(string(c.body))}
		c.rec.Estimated = true
	}
	cost := g.price(c.r.Context(), c, u)
	c.rec.Status = resp.StatusCode
	g.recordCall(c.r.Context(), c)

	setUsageHeaders(c.w.Header(), u, cost)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(resp.StatusCode)
	_, _ = c.w.Write(out) //nolint:gosec // the provider's JSON, served as application/json
	return nil
}

func (g *gateway) messagesTranslated(c *call) error {
	req, err := anthropic.DecodeRequest(c.body)
	if err != nil {
		return badInput(err)
	}
	req.Model = c.resolved.Model
	req.Stream = false
	resp, err := c.resolved.Provider.Chat(upstreamContext(c), req)
	if err != nil {
		return failed(err, false)
	}
	body, err := anthropic.EncodeResponse(resp)
	if err != nil {
		writeAnthropicError(c.w, http.StatusInternalServerError, "", "", "", "could not encode the response")
		return nil //nolint:nilerr // the 500 above is the answer
	}
	cost := g.price(c.r.Context(), c, resp.Usage)
	c.rec.Status = http.StatusOK
	g.recordCall(c.r.Context(), c)

	setUsageHeaders(c.w.Header(), resp.Usage, cost)
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(body) //nolint:gosec // JSON we encoded, served as application/json
	return nil
}

// messagesStreamNative copies an Anthropic stream frame by frame, event names included, and
// reads the usage on the way.
func (g *gateway) messagesStreamNative(c *call, n provider.Native) error {
	body, err := openai.RewriteRequest(c.body, c.resolved.Model, false)
	if err != nil {
		return badInput(err)
	}
	ctx, started, cancel := g.attempt(c.r.Context())
	defer cancel()
	resp, err := n.Native(ctx, &provider.NativeCall{Path: "/messages", Body: body, Header: c.r.Header})
	if err != nil {
		return failed(err, true)
	}
	defer func() { _ = resp.Body.Close() }()
	rd := sse.NewReader(resp.Body)
	ev, err := rd.Next()
	if err == nil && !started() {
		err = errAttemptTimeout
	}
	if err != nil {
		return failed(fmt.Errorf("stream ended before its first event: %w", err), false)
	}

	out := startSSE(c.w)
	var dec anthropic.StreamDecoder
	var upstreamErr *anthropic.StreamError
	var text strings.Builder
	for {
		evs, derr := dec.Events(ev)
		for _, e := range evs {
			text.WriteString(e.Text)
			text.WriteString(e.ToolArgs)
		}
		out.write(sse.Frame(ev.Name, ev.Data))
		if errors.As(derr, &upstreamErr) {
			break
		}
		if ev, err = rd.Next(); err != nil {
			break
		}
	}
	if !dec.Done() {
		if err != nil && !errors.Is(err, io.EOF) {
			g.d.Logger.WarnContext(ctx, "upstream stream broke", "provider", c.rec.Provider, "err", err)
		}
		if upstreamErr == nil {
			out.write(anthropic.ErrorFrame("api_error", streamCut))
		}
		c.rec.Estimated = true
	}
	u := dec.Usage()
	if !dec.Done() {
		u.OutputTokens = max(u.OutputTokens, estimateTokens(text.String()))
	}
	cost := g.price(ctx, c, u)
	out.finish(u, cost)
	c.rec.Status = http.StatusOK
	g.recordCall(ctx, c)
	return nil
}

// messagesStreamTranslated streams from a provider of another dialect as Anthropic events.
func (g *gateway) messagesStreamTranslated(c *call) error {
	req, err := anthropic.DecodeRequest(c.body)
	if err != nil {
		return badInput(err)
	}
	req.Model = c.resolved.Model
	req.Stream = true
	ctx, started, cancel := g.attempt(upstreamContext(c))
	defer cancel()
	st, err := c.resolved.Provider.ChatStream(ctx, req)
	if err != nil {
		return failed(err, false)
	}
	defer func() { _ = st.Close() }()
	ev, err := st.Recv()
	if err == nil && !started() {
		err = errAttemptTimeout
	}
	if err != nil {
		return failed(fmt.Errorf("stream ended before its first event: %w", err), false)
	}

	out := startSSE(c.w)
	enc := anthropic.NewStreamEncoder(anthropic.NewMessageID(), c.resolved.Model)
	var u schema.Usage
	var text strings.Builder
	haveUsage := false
	for {
		if ev.Usage != nil {
			u, haveUsage = *ev.Usage, true
		}
		text.WriteString(ev.Text)
		text.WriteString(ev.ToolArgs)
		for _, f := range enc.Frames(ev) {
			out.write(f)
		}
		if ev, err = st.Recv(); err != nil {
			break
		}
	}
	if errors.Is(err, io.EOF) {
		for _, f := range enc.End() {
			out.write(f)
		}
	} else {
		g.d.Logger.WarnContext(ctx, "provider stream broke", "provider", c.rec.Provider, "err", err)
		out.write(anthropic.ErrorFrame("api_error", streamCut))
	}
	if !haveUsage {
		u = schema.Usage{InputTokens: estimateTokens(string(c.body)), OutputTokens: estimateTokens(text.String())}
		c.rec.Estimated = true
	}
	cost := g.price(ctx, c, u)
	out.finish(u, cost)
	c.rec.Status = http.StatusOK
	g.recordCall(ctx, c)
	return nil
}

// countTokens forwards a token count to the route's Anthropic target. Other providers have no
// equivalent, so a route without one answers 404. Counts are free and not recorded as usage.
func (g *gateway) countTokens(w http.ResponseWriter, r *http.Request) {
	c := g.beginWith(w, r, "messages.count_tokens", writeAnthropicError)
	if c == nil {
		return
	}
	peek, err := anthropic.PeekRequest(c.body)
	if err != nil {
		writeAnthropicRequestError(w, err)
		return
	}
	if !g.resolve(c, peek.Model) {
		return
	}
	// Counting goes to the first target of the route that speaks Anthropic.
	var n provider.Native
	native := false
	for _, t := range c.plan.Targets {
		g.use(c, t, 0)
		if n, native = nativeTarget(c, "anthropic"); native {
			break
		}
	}
	if !native {
		writeAnthropicError(w, http.StatusNotFound, "", "", "", "the route for "+peek.Model+" has no Anthropic target, so it cannot count tokens")
		return
	}
	body, err := openai.RewriteRequest(c.body, c.resolved.Model, false)
	if err != nil {
		writeAnthropicRequestError(w, err)
		return
	}
	resp, err := n.Native(r.Context(), &provider.NativeCall{Path: "/messages/count_tokens", Body: body, Header: r.Header})
	if err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Status >= 400 && pe.Status < 500 && !pe.Retryable {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(pe.Status)
			_, _ = w.Write(pe.Body) //nolint:gosec // the provider's JSON error
			return
		}
		g.d.Logger.WarnContext(r.Context(), "count_tokens failed", "provider", c.rec.Provider, "err", err)
		writeAnthropicError(w, http.StatusBadGateway, "", "", "", "the provider did not answer")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := readUpstream(resp)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "", "", "", "the provider did not answer")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out) //nolint:gosec // the provider's JSON, served as application/json
}
