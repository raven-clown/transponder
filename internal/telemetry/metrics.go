package telemetry

import (
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxCallerModels is how many model names chosen by callers get a series of their own.
const maxCallerModels = 200

// durationBuckets cover a cached answer in milliseconds up to a long stream, in seconds.
var durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

// Metrics are the gateway's Prometheus metrics. No label carries a user, a key or a request
// id, so the number of series follows the config, not the traffic. A nil *Metrics records
// nothing, so the code that reports to it needs no checks.
type Metrics struct {
	reg *Registry

	requests    *Vec
	duration    *Vec
	inflight    *Vec
	limited     *Vec
	dropped     *Vec
	tokens      *Vec
	cost        *Vec
	upstream    *Vec
	fallbacks   *Vec
	unpriced    *Vec
	reloads     *Vec
	authRefused *Vec

	mu           sync.RWMutex
	callerModels map[string]struct{}
}

// NewMetrics returns the gateway metrics, with the build's version as transponder_build_info.
func NewMetrics(version string) *Metrics {
	r := NewRegistry()
	m := &Metrics{
		reg: r,
		requests: r.Counter("transponder_requests_total",
			"Gateway requests answered, by endpoint, route, the provider and model that answered, and HTTP status code.",
			"endpoint", "route", "provider", "model", "code"),
		duration: r.Histogram("transponder_request_duration_seconds",
			"Time from the request arriving to the last byte of the answer, streams included.",
			durationBuckets, "endpoint", "route"),
		inflight: r.Gauge("transponder_inflight_requests", "Requests being served now."),
		limited: r.Counter("transponder_limited_total",
			"Requests refused by a limit or a budget, by the error code of the refusal.", "reason"),
		dropped: r.Counter("transponder_events_dropped_total",
			"Usage records and spans that could not be delivered, by where they were going.", "sink"),
		tokens: r.Counter("transponder_tokens_total",
			"Tokens used, by direction (input, output, cache_read, cache_write, reasoning), provider and model.",
			"direction", "provider", "model"),
		cost: r.Counter("transponder_cost_usd_total", "Cost of the calls in US dollars, by provider and model.",
			"provider", "model"),
		upstream: r.Histogram("transponder_upstream_duration_seconds",
			"Time one attempt on a provider took, to the end of the stream for a streamed call.",
			durationBuckets, "provider", "model"),
		fallbacks: r.Counter("transponder_fallbacks_total",
			"Times a call moved on to the next target of its route, by the providers left and tried next and why.",
			"route", "from", "to", "reason"),
		unpriced: r.Counter("transponder_unpriced_requests_total",
			"Calls on a model that has no price, recorded at a cost of zero."),
		reloads: r.Counter("transponder_config_reloads_total",
			"Reloads of the runtime config, by result: ok for a new version swapped in, failed for one that could not be read or loaded.",
			"result"),
		authRefused: r.Counter("transponder_auth_refused_total",
			"Credentials refused, by reason: missing, unknown_key, disabled_key, expired_key, bad_token, wrong_issuer, wrong_audience or expired_token. No label holds a key, a user or a token.",
			"reason"),
		callerModels: map[string]struct{}{},
	}
	r.Gauge("transponder_build_info", "The running version, as a label on a constant 1.", "version").Set(1, version)
	// Series without labels exist from the start, so a dashboard shows 0 rather than no data.
	m.inflight.Add(0)
	m.unpriced.Add(0)
	for _, reason := range AuthRefusalReasons {
		m.authRefused.Add(0, reason)
	}
	return m
}

// AuthRefusalReasons are the reasons a refused credential is counted under. They match the
// reasons of package auth, which a test checks.
var AuthRefusalReasons = []string{
	"missing", "unknown_key", "disabled_key", "expired_key",
	"bad_token", "wrong_issuer", "wrong_audience", "expired_token",
}

// AuthRefused counts one refused credential, by its reason.
func (m *Metrics) AuthRefused(reason string) {
	if m == nil {
		return
	}
	m.authRefused.Add(1, reason)
}

// Request is what one finished gateway request adds to the metrics.
type Request struct {
	Endpoint string
	Route    string
	Provider string
	Model    string
	// ModelFromCaller is true when Model is the name the caller sent, not one set in the config.
	ModelFromCaller bool
	Code            int
	Duration        time.Duration

	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
	CostNUSD         int64
}

// Inflight changes the number of requests being served by delta.
func (m *Metrics) Inflight(delta int) {
	if m == nil {
		return
	}
	m.inflight.Add(float64(delta))
}

// RequestDone counts a finished request, its duration, and the tokens and cost it used.
func (m *Metrics) RequestDone(r Request) {
	if m == nil {
		return
	}
	model := m.modelLabel(r.Model, r.ModelFromCaller, r.Code/100 == 2)
	m.requests.Add(1, r.Endpoint, r.Route, r.Provider, model, strconv.Itoa(r.Code))
	m.duration.Observe(r.Duration.Seconds(), r.Endpoint, r.Route)
	for _, t := range [...]struct {
		direction string
		n         int64
	}{
		{"input", r.InputTokens},
		{"output", r.OutputTokens},
		{"cache_read", r.CacheReadTokens},
		{"cache_write", r.CacheWriteTokens},
		{"reasoning", r.ReasoningTokens},
	} {
		if t.n > 0 {
			m.tokens.Add(float64(t.n), t.direction, r.Provider, model)
		}
	}
	if r.CostNUSD > 0 {
		m.cost.Add(float64(r.CostNUSD)/1e9, r.Provider, model)
	}
}

// Limited counts a request refused by a limit, by the error code it was refused with.
func (m *Metrics) Limited(reason string) {
	if m == nil {
		return
	}
	m.limited.Add(1, reason)
}

// EventDropped counts a usage record that could not be delivered to sink.
func (m *Metrics) EventDropped(sink string) {
	if m == nil {
		return
	}
	m.dropped.Add(1, sink)
}

// SpansDropped counts n spans lost to a full queue or a collector that did not take them.
func (m *Metrics) SpansDropped(n int) {
	if m == nil {
		return
	}
	m.dropped.Add(float64(n), "otlp")
}

// Unpriced counts a call on a model with no price.
func (m *Metrics) Unpriced() {
	if m == nil {
		return
	}
	m.unpriced.Add(1)
}

// ConfigReloaded counts a reload of the runtime config that was tried.
func (m *Metrics) ConfigReloaded(ok bool) {
	if m == nil {
		return
	}
	result := "failed"
	if ok {
		result = "ok"
	}
	m.reloads.Add(1, result)
}

// Attempt records how long one attempt on a provider took. ok says whether it answered.
func (m *Metrics) Attempt(provider, model string, modelFromCaller, ok bool, d time.Duration) {
	if m == nil {
		return
	}
	m.upstream.Observe(d.Seconds(), provider, m.modelLabel(model, modelFromCaller, ok))
}

// Fallback counts a call that left the provider from for the provider to.
func (m *Metrics) Fallback(route, from, to, reason string) {
	if m == nil {
		return
	}
	m.fallbacks.Add(1, route, from, to, reason)
}

// modelLabel returns the model as a label value. A name from the config is used as it is.
// A name the caller chose gets a series once a provider has answered for it, for at most
// maxCallerModels names, and is counted as "other" until then, so a caller cannot add series
// by sending names that do not exist.
func (m *Metrics) modelLabel(model string, fromCaller, ok bool) string {
	if !fromCaller {
		return model
	}
	m.mu.RLock()
	_, known := m.callerModels[model]
	m.mu.RUnlock()
	if known {
		return model
	}
	if !ok {
		return overflow
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.callerModels) >= maxCallerModels {
		return overflow
	}
	m.callerModels[model] = struct{}{}
	return model
}

// Handler serves the metrics in the Prometheus text format.
func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = m.reg.WriteTo(w)
	})
}
