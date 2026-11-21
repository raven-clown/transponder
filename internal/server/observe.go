package server

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/telemetry"
)

type observedKey struct{}

// observed is what a handler tells the middleware around it about the request.
type observed struct {
	endpoint string
	c        *call
	span     *telemetry.Span // the request's span; nil when tracing is off
}

// observedFrom returns the request's observed, or nil when nothing is observing it.
func observedFrom(ctx context.Context) *observed {
	o, _ := ctx.Value(observedKey{}).(*observed)
	return o
}

// statusWriter remembers the status code a handler answered with.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.code == 0 && code >= http.StatusOK {
		w.code = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach the real writer, which streams need to flush.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// rawWriter returns the server's own writer. http.MaxBytesReader only closes the connection
// after an oversized body when it is given that one.
func rawWriter(w http.ResponseWriter) http.ResponseWriter {
	if sw, ok := w.(*statusWriter); ok {
		return sw.ResponseWriter
	}
	return w
}

// unobserved are the patterns that are not gateway requests: probes and scrapes would only
// add noise to the request counts.
var unobserved = map[string]bool{"/healthz": true, "/readyz": true, "GET /metrics": true}

// observe counts and times the requests that pass through it, and traces each as a span.
// Handlers fill in the rest through the observed on the request context.
func (g *gateway) observe(next http.Handler) http.Handler {
	m, tracer := g.d.Metrics, g.d.Tracer
	if m == nil && tracer == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := g.d.Now()
		o := &observed{span: tracer.Start(r.Method, telemetry.SpanServer, r.Header.Get("Traceparent"), r.Header.Get("Tracestate"))}
		if o.span != nil {
			// Whatever is sent upstream continues the trace under this span. Tracestate goes
			// on as it came.
			r.Header.Set("Traceparent", o.span.Traceparent())
		}
		sw := &statusWriter{ResponseWriter: w}
		r = r.WithContext(context.WithValue(r.Context(), observedKey{}, o))
		returned := false
		m.Inflight(1)
		defer func() {
			m.Inflight(-1)
			// A probe or a scrape is not counted, and its span is left unfinished, so it is
			// never exported.
			if unobserved[r.Pattern] {
				return
			}
			code := sw.code
			switch {
			case code != 0:
			case returned:
				code = http.StatusOK
			default:
				// The handler panicked before it answered, and the server drops the connection.
				code = http.StatusInternalServerError
			}
			req := o.request(code, g.d.Now().Sub(start))
			m.RequestDone(req)
			o.endSpan(r, req)
		}()
		next.ServeHTTP(sw, r)
		returned = true
	})
}

// request describes the finished request for the metrics. The provider and model are left
// empty when no provider was tried, so a refusal is not counted against one.
func (o *observed) request(code int, d time.Duration) telemetry.Request {
	req := telemetry.Request{Endpoint: o.endpoint, Code: code, Duration: d}
	if req.Endpoint == "" {
		req.Endpoint = "other"
	}
	c := o.c
	if c == nil {
		return req
	}
	req.Route = c.rec.Route
	if !c.tried {
		return req
	}
	req.Provider, req.Model, req.ModelFromCaller = c.rec.Provider, c.rec.Model, c.resolved.ModelFromCaller
	req.InputTokens, req.OutputTokens = c.rec.InputTokens, c.rec.OutputTokens
	req.CacheReadTokens, req.CacheWriteTokens = c.rec.CacheReadTokens, c.rec.CacheWriteTokens
	req.ReasoningTokens, req.CostNUSD = c.rec.ReasoningTokens, c.rec.CostNUSD
	return req
}

// endSpan finishes the request's span with what the call turned out to be. It carries
// names, counts and the status, and nothing of the prompt or the answer.
func (o *observed) endSpan(r *http.Request, req telemetry.Request) {
	s := o.span
	// Only a call from a known caller is exported. Someone with no key must not be able to
	// fill the collector, least of all by sending a traceparent that says to sample.
	if s == nil || o.c == nil {
		return
	}
	// The pattern is the method and the path as registered, so the name does not vary
	// with what callers send. A request that matched nothing keeps the bare method.
	if r.Pattern != "" {
		s.SetName(r.Pattern)
		if _, route, ok := strings.Cut(r.Pattern, " "); ok {
			s.SetString("http.route", route)
		}
	}
	s.SetString("http.request.method", r.Method)
	s.SetInt("http.response.status_code", int64(req.Code))
	s.SetString("transponder.endpoint", req.Endpoint)
	s.SetString("transponder.request_id", r.Header.Get(requestIDHeader))
	s.SetString("transponder.route", req.Route)
	s.SetString("transponder.provider", req.Provider)
	s.SetString("transponder.model", req.Model)
	if c := o.c; c != nil && c.tried {
		s.SetString("gen_ai.operation.name", operation(req.Endpoint))
		s.SetInt("transponder.fallbacks", int64(c.rec.Fallbacks))
		setUsage(s, &c.rec)
	}
	if req.Code >= http.StatusInternalServerError {
		s.SetString("error.type", strconv.Itoa(req.Code))
		s.Fail("")
	}
	s.End()
}

// try is one attempt on a target, timed for the metrics and traced as a span under the
// request's.
type try struct {
	g     *gateway
	c     *call
	began time.Time
	span  *telemetry.Span
}

// try starts the attempt on the target the call is using, target i of its plan. Calls the
// adapters make from here on carry the attempt's span as their traceparent, so the
// provider's own spans, if it has any, hang under it.
func (g *gateway) try(c *call, i int) try {
	c.tried = true
	t := try{g: g, c: c, began: g.d.Now()}
	if c.span == nil {
		return t
	}
	op := operation(c.rec.Endpoint)
	t.span = c.span.Child(op+" "+c.resolved.Model, telemetry.SpanClient)
	t.span.SetString("gen_ai.operation.name", op)
	t.span.SetString("gen_ai.provider.name", c.resolved.Type)
	t.span.SetString("gen_ai.request.model", c.resolved.Model)
	t.span.SetString("transponder.provider", c.rec.Provider)
	t.span.SetString("transponder.route", c.plan.Route)
	t.span.SetInt("transponder.attempt", int64(i))
	c.r.Header.Set("Traceparent", t.span.Traceparent())
	return t
}

// answered ends an attempt whose target answered. A stream that broke after its first byte
// answered too, but counts as failed.
func (t try) answered() {
	c := t.c
	t.g.d.Metrics.Attempt(c.rec.Provider, c.resolved.Model, c.resolved.ModelFromCaller, !c.cut, t.g.d.Now().Sub(t.began))
	if t.span == nil {
		return
	}
	t.span.SetInt("http.response.status_code", int64(c.rec.Status))
	setUsage(t.span, &c.rec)
	if c.cut {
		t.span.SetString("error.type", "stream_cut")
		t.span.Fail("the stream broke after it had started")
	}
	t.span.End()
}

// skipped ends an attempt on a target that could not take the request. Nothing was sent to
// it, so it is not timed as an upstream call.
func (t try) skipped() {
	t.span.SetString("error.type", "unsupported")
	t.span.Fail("the target cannot take this request")
	t.span.End()
}

// failed ends an attempt whose target did not answer. The span gets the reason as a fixed
// name and the provider's status code, never the provider's error text, which can quote
// the prompt.
func (t try) failed(err error) {
	c := t.c
	t.g.d.Metrics.Attempt(c.rec.Provider, c.resolved.Model, c.resolved.ModelFromCaller, false, t.g.d.Now().Sub(t.began))
	if t.span == nil {
		return
	}
	reason := fallbackReason(err)
	var pe *provider.Error
	if errors.As(err, &pe) && pe.Status != 0 {
		t.span.SetInt("http.response.status_code", int64(pe.Status))
	}
	t.span.SetString("error.type", reason)
	t.span.Fail(reason)
	t.span.End()
}

// operation names what an endpoint does, in the words of the OpenTelemetry GenAI conventions.
func operation(endpoint string) string {
	switch endpoint {
	case "chat.completions", "messages", "responses":
		return "chat"
	}
	return endpoint
}

// setUsage puts the tokens and cost of a priced call on a span.
func setUsage(s *telemetry.Span, rec *store.UsageRecord) {
	s.SetInt("gen_ai.usage.input_tokens", rec.InputTokens)
	s.SetInt("gen_ai.usage.output_tokens", rec.OutputTokens)
	if rec.CacheReadTokens > 0 {
		s.SetInt("transponder.usage.cache_read_tokens", rec.CacheReadTokens)
	}
	if rec.CacheWriteTokens > 0 {
		s.SetInt("transponder.usage.cache_write_tokens", rec.CacheWriteTokens)
	}
	if rec.ReasoningTokens > 0 {
		s.SetInt("transponder.usage.reasoning_tokens", rec.ReasoningTokens)
	}
	s.SetFloat("transponder.cost_usd", float64(rec.CostNUSD)/1e9)
	if rec.Estimated {
		s.SetBool("transponder.usage.estimated", true)
	}
}

// fellBack counts the move from target i of the plan to the one after it, if there is one.
func (g *gateway) fellBack(c *call, i int, reason string) {
	if i+1 >= len(c.plan.Targets) {
		return
	}
	g.d.Metrics.Fallback(c.plan.Route, c.plan.Targets[i].Provider.Name(), c.plan.Targets[i+1].Provider.Name(), reason)
}

// fallbackReason sorts the error that made a call leave a target into a few fixed names.
func fallbackReason(err error) string {
	var pe *provider.Error
	isProvider := errors.As(err, &pe)
	switch {
	// The caller's own cancel ends the call, so a cancel that leads to a fallback is the
	// attempt clock running out.
	case errors.Is(err, errAttemptTimeout), errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "timeout"
	case !isProvider:
		return "error"
	case pe.Status == 0:
		return "unreachable"
	case pe.Status == http.StatusTooManyRequests:
		return "rate_limited"
	case pe.Status == http.StatusUnauthorized, pe.Status == http.StatusForbidden:
		return "auth"
	case pe.Status == http.StatusRequestTimeout:
		return "timeout"
	case pe.Status >= http.StatusInternalServerError:
		return "server_error"
	}
	return "error"
}

// authFailed answers a request whose credentials were refused or could not be checked.
func (g *gateway) authFailed(w http.ResponseWriter, r *http.Request, err error, writeError errorFunc) {
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		// The reason names claims and key ids, never the token. A trailing-slash
		// issuer is a config hint the operator needs at the default log level,
		// but one line is enough (#173).
		g.d.Logger.DebugContext(r.Context(), "credentials refused", "reason", err.Error())
		if errors.Is(err, auth.ErrIssuerTrailingSlash) {
			g.issuerSlashWarned.Do(func() {
				g.d.Logger.WarnContext(r.Context(), "credentials refused", "reason", err.Error())
			})
		}
		writeError(w, http.StatusUnauthorized, "invalid_request_error", "invalid_api_key", "", "a valid API key or token is required")
	case errors.Is(err, auth.ErrForbidden):
		writeError(w, http.StatusForbidden, "permission_error", "forbidden", "", "this needs the viewer or admin role")
	case errors.Is(err, auth.ErrUnavailable):
		g.d.Logger.WarnContext(r.Context(), "checking a token", "err", err)
		w.Header().Set("Retry-After", "10")
		writeError(w, http.StatusServiceUnavailable, "api_error", "auth_unavailable", "", "the identity provider could not be reached to check the token")
	default:
		g.d.Logger.ErrorContext(r.Context(), "authenticating", "err", err)
		writeError(w, http.StatusInternalServerError, "api_error", "", "", "could not check the credentials")
	}
}

// metrics serves /metrics on the gateway's own port. That port is the one callers reach, and
// the metrics name every provider, model and route and total the spend, so reading them
// there needs a key or token with the viewer role or higher.
func (g *gateway) metrics() http.Handler {
	deny := func(w http.ResponseWriter, r *http.Request, err error) { g.authFailed(w, r, err, openai.WriteError) }
	return g.d.Auth.Middleware(auth.RoleViewer, deny)(g.d.Metrics.Handler())
}

// Ops serves /healthz, /readyz and /metrics for a listener of their own. That listener is
// meant for a private network, so /metrics needs no key there.
func Ops(ready map[string]Pinger, m *telemetry.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/healthz", Healthz())
	mux.Handle("/readyz", Readyz(ready))
	if m != nil {
		mux.Handle("GET /metrics", m.Handler())
	}
	return noSniff(mux)
}
