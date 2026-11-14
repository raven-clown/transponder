package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/tokens"
)

// nativeTarget returns the target as a Native adapter when it speaks the caller's dialect.
func nativeTarget(c *call, dialect string) (provider.Native, bool) {
	if c.resolved.Provider.Dialect() != dialect {
		return nil, false
	}
	n, ok := c.resolved.Provider.(provider.Native)
	return n, ok
}

// attemptError says an attempt on one target failed before anything was written to the caller.
type attemptError struct {
	err error
	// same means the target speaks the caller's dialect, so its error body can be passed on.
	same bool
	// input means the target cannot take this request; nothing was sent to it.
	input bool
}

func (e *attemptError) Error() string { return e.err.Error() }
func (e *attemptError) Unwrap() error { return e.err }

// failed reports a provider call that did not answer.
func failed(err error, same bool) error {
	return &attemptError{err: err, same: same, input: errors.Is(err, provider.ErrUnsupported)}
}

// badInput reports a request this target cannot take as it is.
func badInput(err error) error { return &attemptError{err: err, input: true} }

// retryable reports whether another target may succeed where this one failed.
func retryable(err error) bool {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe.Retryable
	}
	return !errors.Is(err, errUpstreamTooLarge)
}

// run tries the plan's targets in order until one answers. attempt returns nil once it has
// written a response, and an error when its target failed before anything reached the caller.
// It stops at the first error another provider would not fix: the caller's own bad request, a
// caller who has left, or a request past its deadline.
func (g *gateway) run(c *call, attempt func() error) {
	ctx := c.r.Context()
	var upstream, input *attemptError
	var failedTarget router.Resolved
	failedAt := 0
	for i, t := range c.plan.Targets {
		g.use(c, t, i)
		err := attempt()
		if err == nil {
			// A stream that broke after its first byte cannot fall back, but it still counts
			// against the target, unless it broke because the caller left.
			switch {
			case !c.cut:
				g.d.Router.Report(t, true)
			case ctx.Err() == nil:
				g.d.Router.Report(t, false)
			}
			return
		}
		var ae *attemptError
		if !errors.As(err, &ae) {
			ae = &attemptError{err: err}
		}
		if ae.input {
			input = ae
			continue
		}
		upstream, failedTarget, failedAt = ae, t, i
		if ctx.Err() != nil || !retryable(ae.err) {
			break
		}
		g.d.Router.Report(t, false)
		if i < len(c.plan.Targets)-1 {
			g.d.Logger.WarnContext(ctx, "falling back", "route", c.plan.Route, "provider", c.rec.Provider, "model", t.Model, "err", ae.err)
		}
	}
	switch {
	case upstream != nil:
		// An outage outranks a target that could not take the request, and the answer names
		// the target that had it, not one that was skipped after it.
		g.use(c, failedTarget, failedAt)
		g.upstreamFailed(c, upstream.err, upstream.same)
	case input != nil:
		g.rejected(c, input.err)
	}
}

// rejected answers a request no target of the route can take.
func (g *gateway) rejected(c *call, err error) {
	var bad *openai.RequestError
	switch {
	case errors.Is(err, provider.ErrUnsupported):
		g.upstreamFailed(c, err, false)
	case errors.As(err, &bad):
		c.fail(c.w, http.StatusBadRequest, "invalid_request_error", bad.Code, bad.Param, bad.Message)
	default:
		c.fail(c.w, http.StatusBadRequest, "invalid_request_error", "", "", err.Error())
	}
}

// upstreamFailed answers a failed provider call and records it. A bad request from an upstream of
// the caller's dialect is passed on as is, since it would fail the same way anywhere.
func (g *gateway) upstreamFailed(c *call, err error, sameDialect bool) {
	ctx := c.r.Context()
	g.d.Logger.WarnContext(ctx, "provider call failed", "provider", c.rec.Provider, "err", err)
	var pe *provider.Error
	switch {
	case sameDialect && errors.As(err, &pe) && pe.Status >= 400 && pe.Status < 500 && !pe.Retryable:
		setUsageHeaders(c.w.Header(), zeroUsage, 0)
		c.w.Header().Set("Content-Type", "application/json")
		c.w.WriteHeader(pe.Status)
		_, _ = c.w.Write(pe.Body)
		c.rec.Status = pe.Status
	case errors.As(err, &pe) && pe.Status >= 400 && pe.Status < 500 && !pe.Retryable:
		// A request the provider rejects as wrong fails the same way on a retry, so the caller
		// gets the provider's status and reason in its own dialect rather than a retryable 502.
		c.fail(c.w, pe.Status, "invalid_request_error", "upstream_error", "", "the provider rejected the request: "+upstreamMessage(pe.Body))
		c.rec.Status = pe.Status
	case errors.Is(err, errUpstreamTooLarge):
		c.fail(c.w, http.StatusBadGateway, "api_error", "upstream_too_large", "", "the provider's response is larger than 64 MiB; ask for fewer inputs or base64 encoding")
		c.rec.Status = http.StatusBadGateway
	case errors.Is(err, provider.ErrUnsupported):
		c.fail(c.w, http.StatusBadRequest, "invalid_request_error", "unsupported", "model", "the provider for this model cannot serve this request")
		c.rec.Status = http.StatusBadRequest
	default:
		c.fail(c.w, http.StatusBadGateway, "api_error", "upstream_error", "", "the provider did not answer")
		c.rec.Status = http.StatusBadGateway
	}
	g.recordCall(ctx, c)
}

var errUpstreamTooLarge = errors.New("the provider's response is larger than the gateway accepts")

func readUpstream(resp *http.Response) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBytes+1))
	if err == nil && int64(len(b)) > maxUpstreamBytes {
		return nil, errUpstreamTooLarge
	}
	return b, err
}

func estimateTokens(s string) int64 {
	return tokens.Estimate(s)
}

// upstreamMessage reads error.message, which OpenAI, Anthropic and Gemini all use, from an
// upstream error body.
func upstreamMessage(body []byte) string {
	var w struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &w) == nil && w.Error.Message != "" {
		return w.Error.Message
	}
	const limit = 200
	if len(body) > limit {
		body = body[:limit]
	}
	return strings.TrimSpace(string(body))
}
