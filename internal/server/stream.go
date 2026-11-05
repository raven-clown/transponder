package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
)

var usageTrailers = strings.Join([]string{"X-Usage-Input-Tokens", "X-Usage-Output-Tokens", "X-Usage-Cost-USD"}, ", ")

// sse writes an event stream whose usage arrives as trailers.
type sse struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func startSSE(w http.ResponseWriter) *sse {
	h := w.Header()
	h.Set("Trailer", usageTrailers)
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return &sse{w: w, rc: http.NewResponseController(w)}
}

func (s *sse) write(frame []byte) {
	_, _ = s.w.Write(frame) //nolint:gosec // event stream frames, served as text/event-stream
	_ = s.rc.Flush()
}

func (s *sse) data(payload []byte) {
	var frame []byte
	for line := range bytes.SplitSeq(payload, []byte{'\n'}) {
		frame = append(frame, "data: "...)
		frame = append(frame, line...)
		frame = append(frame, '\n')
	}
	s.write(append(frame, '\n'))
}

func (s *sse) finish(u schema.Usage, cost int64) {
	setUsageHeaders(s.w.Header(), u, cost)
}

func (g *gateway) chatStreamNative(c *call, n provider.Native, includeUsage bool) {
	ctx := c.r.Context()
	body, err := openai.RewriteRequest(c.body, c.resolved.Model, true)
	if err != nil {
		writeRequestError(c.w, &openai.RequestError{Code: "invalid_json", Message: err.Error()})
		return
	}
	resp, err := n.Native(ctx, &provider.NativeCall{Path: "/chat/completions", Body: body, Header: c.r.Header})
	if err != nil {
		g.upstreamFailed(c, err, true)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	rd := openai.NewSSEReader(resp.Body)
	first, err := rd.Next()
	if err != nil {
		g.upstreamFailed(c, fmt.Errorf("stream ended before its first event: %w", err), false)
		return
	}

	out := startSSE(c.w)
	var u schema.Usage
	var text strings.Builder
	haveUsage := false
	pass := func(data []byte) {
		cu, ok, only := openai.ChunkUsage(data)
		if ok {
			u, haveUsage = cu, true
		}
		text.WriteString(deltaText(data))
		if only && !includeUsage {
			return
		}
		out.data(data)
	}
	pass(first)
	for {
		data, err := rd.Next()
		if errors.Is(err, io.EOF) {
			out.write([]byte(openai.DoneFrame))
			break
		}
		if err != nil {
			g.d.Logger.WarnContext(ctx, "upstream stream broke", "provider", c.rec.Provider, "err", err)
			out.write(openai.EncodeStreamError("upstream_error", "the provider stopped in the middle of the stream"))
			break
		}
		pass(data)
	}
	if !haveUsage {
		u = schema.Usage{InputTokens: estimateTokens(promptText(c.body)), OutputTokens: estimateTokens(text.String())}
		c.rec.Estimated = true
	}
	cost := g.price(ctx, c, u)
	out.finish(u, cost)
	c.rec.Status = http.StatusOK
	g.record(ctx, c.rec)
}

func (g *gateway) chatStreamCanonical(c *call, includeUsage bool) {
	ctx := c.r.Context()
	req, err := openai.ParseChatRequest(c.body)
	if err != nil {
		writeRequestError(c.w, err)
		return
	}
	req.Model = c.resolved.Model
	req.Stream = true
	st, err := c.resolved.Provider.ChatStream(ctx, req)
	if err != nil {
		g.upstreamFailed(c, err, false)
		return
	}
	defer func() { _ = st.Close() }()
	first, err := st.Recv()
	if err != nil {
		g.upstreamFailed(c, fmt.Errorf("stream ended before its first event: %w", err), false)
		return
	}

	id := "chatcmpl-" + randomHex()
	out := startSSE(c.w)
	var u schema.Usage
	var text strings.Builder
	haveUsage := false
	ev := first
	for {
		if ev.Usage != nil {
			u, haveUsage = *ev.Usage, true
		}
		text.WriteString(ev.Text)
		if ev.Type != schema.EventUsage || includeUsage {
			frame, err := openai.EncodeChunk(id, c.resolved.Model, c.rec.Time, ev)
			if err == nil && frame != nil {
				out.write(frame)
			}
		}
		ev, err = st.Recv()
		if errors.Is(err, io.EOF) {
			out.write([]byte(openai.DoneFrame))
			break
		}
		if err != nil {
			g.d.Logger.WarnContext(ctx, "provider stream broke", "provider", c.rec.Provider, "err", err)
			out.write(openai.EncodeStreamError("upstream_error", "the provider stopped in the middle of the stream"))
			break
		}
	}
	if !haveUsage {
		u = schema.Usage{InputTokens: estimateTokens(promptText(c.body)), OutputTokens: estimateTokens(text.String())}
		c.rec.Estimated = true
	}
	cost := g.price(ctx, c, u)
	out.finish(u, cost)
	c.rec.Status = http.StatusOK
	g.record(ctx, c.rec)
}

// deltaText joins the content deltas of one stream chunk.
func deltaText(data []byte) string {
	var w struct {
		Choices []struct {
			Delta struct {
				Content string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(data, &w) != nil {
		return ""
	}
	var b strings.Builder
	for _, ch := range w.Choices {
		b.WriteString(ch.Delta.Content)
	}
	return b.String()
}

func randomHex() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
