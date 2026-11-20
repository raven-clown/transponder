// Package server serves the gateway endpoints.
package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/dialect/anthropic"
	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/pricing"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/schema"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/telemetry"
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
	// AttemptTimeout bounds the wait for the first event of a stream. Zero means no limit.
	AttemptTimeout time.Duration
	// RequestTimeout bounds each call, streams included. Zero means no limit.
	RequestTimeout time.Duration
	// Limiter enforces budgets, token limits and request limits. Nil means none.
	Limiter *limits.Limiter
	// Live, when set, returns the router and limiter to use in place of Router and Limiter. It
	// is read once when a call starts, so a config reload never changes a call halfway.
	Live func() (*router.Router, *limits.Limiter)
	// Ready are the dependencies /readyz pings, by name.
	Ready map[string]Pinger
	// Metrics counts requests, tokens, cost, fallbacks and refusals, and is served on
	// /metrics. Nil means nothing is counted and there is no /metrics.
	Metrics *telemetry.Metrics
	// Tracer makes a span for each call and each attempt on a provider. Nil means no tracing.
	Tracer *telemetry.Tracer
	// Admin, when set, serves every path under /admin/v1/.
	Admin http.Handler
}

// New returns the gateway handler.
func New(d Deps) http.Handler {
	g := &gateway{d: d}
	mux := http.NewServeMux()
	if d.Metrics != nil {
		mux.Handle("GET /metrics", g.metrics())
	}
	mux.HandleFunc("POST /v1/chat/completions", g.chat)
	mux.HandleFunc("POST /v1/responses", g.responses)
	mux.HandleFunc("POST /v1/embeddings", g.embeddings)
	mux.HandleFunc("GET /v1/models", g.models)
	mux.HandleFunc("GET /v1/usage", g.usage)
	mux.HandleFunc("POST /v1/messages", g.messages)
	mux.HandleFunc("POST /v1/messages/count_tokens", g.countTokens)
	mux.Handle("/healthz", Healthz())
	mux.Handle("/readyz", Readyz(d.Ready))
	if d.Admin != nil {
		mux.Handle("/admin/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if o := observedFrom(r.Context()); o != nil {
				o.endpoint = "admin"
			}
			d.Admin.ServeHTTP(w, r)
		}))
	}
	h := noSniff(g.observe(mux))
	if d.RequestTimeout <= 0 {
		return RequestID(h)
	}
	return RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d.RequestTimeout)
		defer cancel()
		h.ServeHTTP(w, r.WithContext(ctx))
	}))
}

type gateway struct {
	d Deps
	// issuerSlashWarned keeps the trailing-slash hint from flooding the log.
	issuerSlashWarned sync.Once
}

// call is one gateway request on its way through.
type call struct {
	w        http.ResponseWriter
	r        *http.Request
	body     []byte
	rec      store.UsageRecord
	plan     router.Plan
	resolved router.Resolved // the target being tried
	cut      bool            // the answering target's stream broke after it had started
	tried    bool            // a provider was asked, so the metrics name it
	span     *telemetry.Span // the request's span; nil when tracing is off
	fail     errorFunc
	id       auth.Identity
	caller   limits.Caller
	ticket   *limits.Ticket
	router   *router.Router  // the config the call started on
	limiter  *limits.Limiter // nil when nothing is limited
}

// errorFunc writes an error in the caller's dialect.
type errorFunc func(w http.ResponseWriter, status int, errType, code, param, message string)

// begin starts an OpenAI-dialect call.
func (g *gateway) begin(w http.ResponseWriter, r *http.Request, endpoint string) *call {
	return g.beginWith(w, r, endpoint, writeError)
}

// beginWith authenticates and reads the body. It writes the error response itself, in the
// caller's dialect, and returns nil when the call cannot go on.
func (g *gateway) beginWith(w http.ResponseWriter, r *http.Request, endpoint string, writeError errorFunc) *call {
	start := g.d.Now()
	o := observedFrom(r.Context())
	if o != nil {
		o.endpoint = endpoint
	}
	id, err := g.d.Auth.Authenticate(r)
	if err != nil {
		g.authFailed(w, r, err, writeError)
		return nil
	}
	if r.ContentLength > maxBodyBytes {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", "", "the request body is larger than 20 MiB")
		return nil
	}
	body, err := io.ReadAll(http.MaxBytesReader(rawWriter(w), r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", "", "the request body is larger than 20 MiB")
		} else {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "", "", "could not read the request body")
		}
		return nil
	}
	// Handlers and adapters read the caller from the request context.
	r = r.WithContext(auth.WithIdentity(r.Context(), id))
	rt, limiter := g.live()
	c := &call{w: w, r: r, body: body, fail: writeError, id: id, router: rt, limiter: limiter, caller: limits.Caller{User: id.User, Teams: id.Teams, Key: id.Key}, rec: store.UsageRecord{
		OrgID:     id.OrgID,
		Time:      start,
		RequestID: r.Header.Get(requestIDHeader),
		User:      id.User,
		Teams:     id.Teams,
		Key:       id.Key,
		Project:   r.Header.Get("X-Project"),
		Endpoint:  endpoint,
	}}
	if o != nil {
		o.c, c.span = c, o.span
	}
	return c
}

// live returns the router and limiter a new call uses.
func (g *gateway) live() (*router.Router, *limits.Limiter) {
	if g.d.Live != nil {
		return g.d.Live()
	}
	return g.d.Router, g.d.Limiter
}

// resolve finds the targets for model, or writes 404 or 403 and records nothing.
func (g *gateway) resolve(c *call, model string) bool {
	plan, err := c.router.Plan(model)
	if err != nil {
		c.fail(c.w, http.StatusNotFound, "invalid_request_error", "model_not_found", "model", "no route serves model "+model)
		return false
	}
	if !plan.Access.Permits(c.id) {
		c.fail(c.w, http.StatusForbidden, "permission_error", "route_forbidden", "model",
			fmt.Sprintf("route %q does not allow this caller to use model %s", plan.Route, model))
		return false
	}
	c.plan = plan
	c.rec.Route = plan.Route
	g.use(c, plan.Targets[0], 0)
	return true
}

// use makes t the target being tried, after fallbacks earlier targets failed.
func (g *gateway) use(c *call, t router.Resolved, fallbacks int) {
	c.resolved = t
	c.rec.Provider = t.Provider.Name()
	c.rec.Model = t.Model
	c.rec.Fallbacks = fallbacks
	h := c.w.Header()
	h.Set("X-Transponder-Provider", c.rec.Provider)
	h.Set("X-Transponder-Model", t.Model)
	h.Set("X-Transponder-Fallbacks", strconv.Itoa(fallbacks))
}

// price fills the usage and cost of the record and returns the cost.
func (g *gateway) price(ctx context.Context, c *call, u schema.Usage) int64 {
	c.rec.InputTokens = u.InputTokens
	c.rec.OutputTokens = u.OutputTokens
	c.rec.CacheReadTokens = u.CacheReadTokens
	c.rec.CacheWriteTokens = u.CacheWriteTokens
	c.rec.ReasoningTokens = u.ReasoningTokens
	if c.resolved.Free {
		c.rec.CostNUSD = 0
		return 0
	}
	cost, priced := g.d.Prices.Cost(c.resolved.Type, c.resolved.Model, u)
	if !priced {
		g.d.Logger.InfoContext(ctx, "unpriced", "provider", c.rec.Provider, "model", c.rec.Model)
		g.d.Metrics.Unpriced()
	}
	c.rec.CostNUSD = cost
	return cost
}

func (g *gateway) record(ctx context.Context, rec store.UsageRecord) {
	rec.Latency = g.d.Now().Sub(rec.Time)
	if err := g.d.Usage.RecordUsage(context.WithoutCancel(ctx), rec); err != nil {
		g.d.Logger.ErrorContext(ctx, "recording usage", "request_id", rec.RequestID, "err", err)
		g.d.Metrics.EventDropped("sql")
	}
}

func writeError(w http.ResponseWriter, status int, errType, code, param, message string) {
	setUsageHeaders(w.Header(), schema.Usage{}, 0)
	openai.WriteError(w, status, errType, code, param, message)
}

// writeAnthropicError writes the same errors in the Anthropic shape, where the type follows
// from the status.
func writeAnthropicError(w http.ResponseWriter, status int, _, code, _, message string) {
	setUsageHeaders(w.Header(), schema.Usage{}, 0)
	anthropic.WriteErrorCode(w, status, anthropic.ErrorType(status), code, message)
}

// upstreamContext carries the caller's trace headers to adapters called through the canonical methods.
func upstreamContext(c *call) context.Context {
	return provider.WithHeader(c.r.Context(), c.r.Header)
}
