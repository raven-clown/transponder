package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/schema"
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

// statusOverloaded is Anthropic's status for a provider that is over capacity, which is a
// rate limit in everything but name: waiting is the only answer that helps.
const statusOverloaded = 529

// rateLimitWait reports the instant a failed attempt said the caller could retry, and
// whether it was a rate limit at all. The instant is zero when the provider asked for no wait.
// A 429 that names a spent quota is not a rate limit: OpenAI answers an account with no credit
// left with that status and error.code insufficient_quota, which no wait clears, so it stays a
// failure the caller has to fix rather than one to retry.
func rateLimitWait(err error) (time.Time, bool) {
	var pe *provider.Error
	if errors.As(err, &pe) && (pe.Status == http.StatusTooManyRequests || pe.Status == statusOverloaded) {
		if quotaExhausted(pe.Body) {
			return time.Time{}, false
		}
		return pe.RetryAt, true
	}
	return time.Time{}, false
}

// quotaExhausted reports whether an upstream error body names a spent quota rather than a
// caller coming too fast. OpenAI sets error.code to insufficient_quota for it, and some of its
// errors carry the same word in error.type instead.
func quotaExhausted(body []byte) bool {
	var w struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &w) != nil {
		return false
	}
	return w.Error.Code == "insufficient_quota" || w.Error.Type == "insufficient_quota"
}

// earliestRetry is the earlier of the deadline already recorded and asked. A deadline is an
// instant rather than a duration because the caller is told the wait when the answer is
// written, which can be long after the attempt that set it. A deadline already past is still
// the earliest, so a provider that is ready now is not outwaited by a later one that is not.
func earliestRetry(deadline, asked time.Time) time.Time {
	switch {
	case asked.IsZero():
		return deadline
	case deadline.IsZero() || asked.Before(deadline):
		return asked
	}
	return deadline
}

// timeLeft is how long remains until deadline, or zero when it has passed or was never set.
func timeLeft(now, deadline time.Time) time.Duration {
	if deadline.IsZero() {
		return 0
	}
	if left := deadline.Sub(now); left > 0 {
		return left
	}
	return 0
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
	// rateLimited says every attempt so far was answered with 429 or 529, and retryBy is the
	// earliest deadline any of them gave. The time spent on the other attempts comes off the
	// wait the caller is told about, since a Retry-After is a deadline, not a delay to restart.
	rateLimited := true
	var retryBy time.Time
	for i, t := range c.plan.Targets {
		g.use(c, t, i)
		try := g.try(c, i)
		err := attempt()
		if err == nil {
			try.answered()
			// A stream that broke after its first byte cannot fall back, but it still counts
			// against the target, unless it broke because the caller left. One that stalled
			// until the request timeout counts.
			switch {
			case !c.cut:
				c.router.Report(t, true)
			case !callerGone(c):
				c.router.Report(t, false)
			}
			return
		}
		var ae *attemptError
		if !errors.As(err, &ae) {
			ae = &attemptError{err: err}
		}
		if ae.input {
			input = ae
			// A target that could not take the request never answered, so the caller is not
			// told that every provider is rate limiting when one of them was skipped.
			rateLimited = false
			try.skipped()
			g.fellBack(c, i, "unsupported")
			continue
		}
		try.failed(ae.err)
		upstream, failedTarget, failedAt = ae, t, i
		if at, ok := rateLimitWait(ae.err); ok {
			retryBy = earliestRetry(retryBy, at)
		} else {
			rateLimited = false
		}
		if ctx.Err() != nil || !retryable(ae.err) {
			break
		}
		c.router.Report(t, false)
		if i < len(c.plan.Targets)-1 {
			g.d.Logger.WarnContext(ctx, "falling back", "route", c.plan.Route, "provider", c.rec.Provider, "model", t.Model, "err", ae.err)
		}
		g.fellBack(c, i, fallbackReason(ae.err))
	}
	switch {
	case upstream != nil:
		// An outage outranks a target that could not take the request, and the answer names
		// the target that had it, not one that was skipped after it.
		g.use(c, failedTarget, failedAt)
		if rateLimited {
			g.rateLimited(c, timeLeft(g.d.Now(), retryBy))
			return
		}
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
	if callerGone(c) {
		g.d.Logger.InfoContext(ctx, "caller left before the answer", "provider", c.rec.Provider)
		c.w.WriteHeader(statusCallerGone)
		c.rec.Status = statusCallerGone
		g.recordCall(ctx, c)
		return
	}
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
		msg := "the provider for this model cannot serve this request"
		// Name the content that could not be carried, so the caller knows what to change.
		var ce *schema.ContentError
		if errors.As(err, &ce) {
			msg += ": " + ce.Reason
		}
		c.fail(c.w, http.StatusBadRequest, "invalid_request_error", "unsupported", "model", msg)
		c.rec.Status = http.StatusBadRequest
	default:
		c.fail(c.w, http.StatusBadGateway, "api_error", "upstream_error", "", "no provider for this model could answer")
		c.rec.Status = http.StatusBadGateway
	}
	g.recordCall(ctx, c)
}

// rateLimited answers a call whose every attempt was rate limiting, with the shortest wait
// a provider asked for, so an SDK backs off instead of treating the gateway as broken.
func (g *gateway) rateLimited(c *call, wait time.Duration) {
	ctx := c.r.Context()
	if callerGone(c) {
		g.d.Logger.InfoContext(ctx, "caller left before the answer", "provider", c.rec.Provider)
		c.w.WriteHeader(statusCallerGone)
		c.rec.Status = statusCallerGone
		g.recordCall(ctx, c)
		return
	}
	if wait <= 0 {
		wait = time.Second
	}
	secs := int((wait + time.Second - 1) / time.Second)
	g.d.Logger.WarnContext(ctx, "every provider is rate limiting", "provider", c.rec.Provider, "retry_after", secs)
	c.w.Header().Set("Retry-After", strconv.Itoa(secs))
	c.fail(c.w, http.StatusTooManyRequests, "api_error", "upstream_rate_limited", "",
		"every provider for this model is rate limiting; retry after "+waitText(secs))
	c.rec.Status = http.StatusTooManyRequests
	g.recordCall(ctx, c)
}

// waitText says how long to wait, in whole seconds, in a sentence.
func waitText(secs int) string {
	if secs == 1 {
		return "1 second"
	}
	return strconv.Itoa(secs) + " seconds"
}

// statusCallerGone records a call whose caller closed the connection before the answer ended,
// as nginx does.
const statusCallerGone = 499

// callerGone reports whether the caller hung up. A request that ran out of time ends with
// DeadlineExceeded instead, and counts as the provider's failure.
func callerGone(c *call) bool {
	return errors.Is(c.r.Context().Err(), context.Canceled)
}

// streamBroke handles a stream that stopped before its end, after the caller had its first
// byte. It reports whether to send the caller an error event; a caller who left gets none.
func (g *gateway) streamBroke(ctx context.Context, c *call, err error) bool {
	c.cut = true
	if callerGone(c) {
		g.d.Logger.InfoContext(ctx, "caller left in the middle of the stream", "provider", c.rec.Provider)
		return false
	}
	g.d.Logger.WarnContext(ctx, "upstream stream broke", "provider", c.rec.Provider, "err", err)
	return true
}

// streamStatus is the status recorded for a stream that began.
func streamStatus(c *call) int {
	if c.cut && callerGone(c) {
		return statusCallerGone
	}
	return http.StatusOK
}

var errUpstreamTooLarge = errors.New("the provider's response is larger than the gateway accepts")

func readUpstream(resp *http.Response) ([]byte, error) {
	b, err := provider.ReadBody(io.LimitReader(resp.Body, maxUpstreamBytes+1), resp.ContentLength, maxUpstreamBytes+1)
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
