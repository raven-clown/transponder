// Package server serves the gateway endpoints.
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/pricing"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/store"
)

const maxBodyBytes = 20 << 20

var maxUpstreamBytes int64 = 64 << 20

// Deps are what the server needs to answer requests.
type Deps struct {
	Auth   *auth.Authenticator
	Router *router.Router
	Prices *pricing.Table
	Usage  store.UsageStore
	Logger *slog.Logger
	Now    func() time.Time
}

// New returns the gateway handler.
func New(d Deps) http.Handler {
	g := &gateway{d: d}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", g.chat)
	mux.HandleFunc("POST /v1/embeddings", g.embeddings)
	return mux
}

type gateway struct {
	d Deps
}

// call is one gateway request on its way through.
type call struct {
	w        http.ResponseWriter
	r        *http.Request
	body     []byte
	rec      store.UsageRecord
	resolved router.Resolved
}

// begin authenticates and reads the body. It writes the error response itself and returns nil when the call cannot go on.
func (g *gateway) begin(w http.ResponseWriter, r *http.Request, endpoint string) *call {
	start := g.d.Now()
	id, err := g.d.Auth.Authenticate(r)
	if err != nil {
		if !errors.Is(err, auth.ErrUnauthenticated) {
			g.d.Logger.ErrorContext(r.Context(), "authenticating", "err", err)
			writeError(w, http.StatusInternalServerError, "api_error", "", "", "could not check the API key")
			return nil
		}
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "", "a valid API key is required")
		return nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", "", "the request body is larger than 20 MiB")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "", "", "could not read the request body")
		}
		return nil
	}
	return &call{w: w, r: r, body: body, rec: store.UsageRecord{
		OrgID:     id.OrgID,
		Time:      start,
		RequestID: r.Header.Get("X-Request-ID"),
		User:      id.User,
		Teams:     id.Teams,
		Key:       id.Key,
		Project:   r.Header.Get("X-Project"),
		Endpoint:  endpoint,
	}}
}

// resolve picks the target for model, or writes 404 and records nothing.
func (g *gateway) resolve(c *call, model string) bool {
	res, err := g.d.Router.Resolve(model)
	if err != nil {
		writeError(c.w, http.StatusNotFound, "invalid_request_error", "model_not_found", "model", "no route serves model "+model)
		return false
	}
	c.resolved = res
	c.rec.Route = res.Route
	c.rec.Provider = res.Provider.Name()
	c.rec.Model = res.Model
	return true
}

// price fills the usage and cost of the record and returns the cost.
func (g *gateway) price(ctx context.Context, c *call, u schema.Usage) int64 {
	c.rec.InputTokens = u.InputTokens
	c.rec.OutputTokens = u.OutputTokens
	c.rec.CacheReadTokens = u.CacheReadTokens
	c.rec.CacheWriteTokens = u.CacheWriteTokens
	cost, priced := g.d.Prices.Cost(c.resolved.Type, c.resolved.Model, u)
	if !priced {
		g.d.Logger.InfoContext(ctx, "unpriced", "provider", c.rec.Provider, "model", c.rec.Model)
	}
	c.rec.CostNUSD = cost
	return cost
}

func (g *gateway) record(ctx context.Context, rec store.UsageRecord) {
	rec.Latency = g.d.Now().Sub(rec.Time)
	if err := g.d.Usage.RecordUsage(context.WithoutCancel(ctx), rec); err != nil {
		g.d.Logger.ErrorContext(ctx, "recording usage", "request_id", rec.RequestID, "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, errType, code, param, message string) {
	setUsageHeaders(w.Header(), schema.Usage{}, 0)
	openai.WriteError(w, status, errType, code, param, message)
}
