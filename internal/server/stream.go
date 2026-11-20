package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/sse"
)

var usageTrailers = strings.Join([]string{"X-Usage-Input-Tokens", "X-Usage-Output-Tokens", "X-Usage-Reasoning-Tokens", "X-Usage-Cost-USD"}, ", ")

// sse writes an event stream whose usage arrives as trailers.
type sseWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func startSSE(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("Trailer", usageTrailers)
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

func (s *sseWriter) write(frame []byte) {
	_, _ = s.w.Write(frame) //nolint:gosec // event stream frames, served as text/event-stream
	_ = s.rc.Flush()
}

func (s *sseWriter) data(payload []byte) {
	s.write(sse.Frame("", payload))
}

func (s *sseWriter) finish(u schema.Usage, cost int64) {
	setUsageHeaders(s.w.Header(), u, cost)
}

func (g *gateway) chatStreamNative(c *call, n provider.Native, includeUsage bool) error {
	ctx := c.r.Context()
	body, err := openai.RewriteRequest(c.body, c.resolved.Model, true)
	if err != nil {
		return badInput(&openai.RequestError{Code: "invalid_json", Message: err.Error()})
	}
	body = openai.ShortenToolCallIDs(body)
	ctx, started, cancel := g.attempt(ctx)
	defer cancel()
	resp, err := n.Native(ctx, &provider.NativeCall{Path: "/chat/completions", Body: body, Header: c.r.Header})
	if err != nil {
		return failed(err, true)
	}
	defer func() { _ = resp.Body.Close() }()
	next, err := open(clocked(openai.NewSSEReader(resp.Body).Next, started), openai.OpeningChunk, func(data []byte) error {
		if openai.IsStreamError(data) {
			return fmt.Errorf("the provider began the stream with an error: %s", upstreamMessage(data))
		}
		return nil
	})
	if err != nil {
		return failed(fmt.Errorf("stream ended before its first output: %w", err), false)
	}

	out := startSSE(c.w)
	var u schema.Usage
	var text strings.Builder
	haveUsage := false
	pass := func(data []byte) {
		delta, cu, ok, only := openai.ReadChunk(data)
		if ok && reported(cu) {
			u, haveUsage = cu, true
		}
		text.WriteString(delta)
		if only && !includeUsage {
			return
		}
		out.data(data)
	}
	for {
		data, err := next()
		if errors.Is(err, io.EOF) {
			out.write([]byte(openai.DoneFrame))
			break
		}
		if err != nil {
			if g.streamBroke(ctx, c, err) {
				out.write(openai.EncodeStreamError("upstream_error", streamCut))
			}
			break
		}
		pass(data)
	}
	if !haveUsage {
		u = schema.Usage{InputTokens: promptTokens(c.body), OutputTokens: estimateTokens(text.String())}
		c.rec.Estimated = true
	}
	cost := g.price(ctx, c, u)
	out.finish(u, cost)
	c.rec.Status = streamStatus(c)
	g.recordCall(ctx, c)
	return nil
}

func (g *gateway) chatStreamCanonical(c *call, includeUsage bool) error {
	req, err := openai.ParseChatRequest(c.body)
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
	next, err := open(clocked(st.Recv, started), startEvent, noFailure)
	if err != nil {
		return failed(fmt.Errorf("stream ended before its first output: %w", err), false)
	}

	id := "chatcmpl-" + randomHex()
	out := startSSE(c.w)
	var u schema.Usage
	var text strings.Builder
	haveUsage := false
	for {
		ev, err := next()
		if errors.Is(err, io.EOF) {
			out.write([]byte(openai.DoneFrame))
			break
		}
		if err != nil {
			if g.streamBroke(ctx, c, err) {
				out.write(openai.EncodeStreamError("upstream_error", streamCut))
			}
			break
		}
		if ev.Usage != nil && reported(*ev.Usage) {
			u, haveUsage = *ev.Usage, true
		}
		text.WriteString(ev.Text)
		text.WriteString(ev.ToolName)
		text.WriteString(ev.ToolArgs)
		if ev.Type != schema.EventUsage || includeUsage {
			frame, err := openai.EncodeChunk(id, c.resolved.Model, c.rec.Time, ev)
			if err == nil && frame != nil {
				out.write(frame)
			}
		}
	}
	if !haveUsage {
		u = schema.Usage{InputTokens: promptTokens(c.body), OutputTokens: estimateTokens(text.String())}
		c.rec.Estimated = true
	}
	cost := g.price(ctx, c, u)
	out.finish(u, cost)
	c.rec.Status = streamStatus(c)
	g.recordCall(ctx, c)
	return nil
}

var errAttemptTimeout = errors.New("the provider did not start the stream within the attempt timeout")

// clocked stops the attempt clock at the first event read, and fails that read when the event
// came too late.
func clocked[T any](read func() (T, error), started func() bool) func() (T, error) {
	first := true
	return func() (T, error) {
		ev, err := read()
		if first {
			first = false
			if err == nil && !started() {
				err = errAttemptTimeout
			}
		}
		return ev, err
	}
}

// startEvent reports whether a translated stream event is the start of the message, which
// carries no output.
func startEvent(ev schema.Event) bool { return ev.Type == schema.EventStart }

// attempt returns a context that is cancelled when the first event takes longer than the
// attempt timeout. started stops the clock and reports whether the event came in time.
// Calls that are not streamed have no attempt clock, since their headers arrive only once
// the whole answer is ready; the request timeout bounds them.
func (g *gateway) attempt(ctx context.Context) (actx context.Context, started func() bool, cancel context.CancelFunc) {
	actx, cancel = context.WithCancel(ctx)
	if g.d.AttemptTimeout <= 0 {
		return actx, func() bool { return true }, cancel
	}
	timer := time.AfterFunc(g.d.AttemptTimeout, cancel)
	return actx, timer.Stop, cancel
}

func randomHex() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
