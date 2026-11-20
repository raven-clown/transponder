package server

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/dialect/openai"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/telemetry"
)

type observedKey struct{}

// observed is what a handler tells the middleware around it about the request.
type observed struct {
	endpoint string
	c        *call
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

// observe counts and times the requests that pass through it. Handlers fill in the rest
// through the observed on the request context.
func (g *gateway) observe(next http.Handler) http.Handler {
	m := g.d.Metrics
	if m == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := g.d.Now()
		o := &observed{}
		sw := &statusWriter{ResponseWriter: w}
		r = r.WithContext(context.WithValue(r.Context(), observedKey{}, o))
		returned := false
		m.Inflight(1)
		defer func() {
			m.Inflight(-1)
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
			m.RequestDone(o.request(code, g.d.Now().Sub(start)))
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

// attempted records how long the attempt on the target being tried took.
func (g *gateway) attempted(c *call, began time.Time, ok bool) {
	g.d.Metrics.Attempt(c.rec.Provider, c.resolved.Model, c.resolved.ModelFromCaller, ok, g.d.Now().Sub(began))
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
		// The reason names claims and key ids, never the token.
		g.d.Logger.DebugContext(r.Context(), "credentials refused", "reason", err.Error())
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
	return mux
}
