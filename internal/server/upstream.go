package server

import (
	"errors"
	"io"
	"net/http"

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
	case errors.Is(err, provider.ErrUnsupported):
		writeError(c.w, http.StatusBadRequest, "invalid_request_error", "unsupported", "model", "the provider for this model cannot serve this request")
		c.rec.Status = http.StatusBadRequest
	default:
		writeError(c.w, http.StatusBadGateway, "api_error", "upstream_error", "", "the provider did not answer")
		c.rec.Status = http.StatusBadGateway
	}
	g.record(ctx, c.rec)
}

func readUpstream(resp *http.Response) ([]byte, error) {
	return io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBytes))
}

func estimateTokens(s string) int64 {
	n := int64(len([]rune(s)))
	return (n + 3) / 4
}
