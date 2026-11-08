package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/raven-clown/transponder/internal/provider"
)

// nativeTarget returns the target as a Native adapter when it speaks the caller's dialect.
func nativeTarget(c *call, dialect string) (provider.Native, bool) {
	if c.resolved.Provider.Dialect() != dialect {
		return nil, false
	}
	n, ok := c.resolved.Provider.(provider.Native)
	return n, ok
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
	g.record(ctx, c.rec)
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
	n := int64(len([]rune(s)))
	return (n + 3) / 4
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
