package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("content type %q", ct)
	}
	return rec.Body.String()
}

func wantLines(t *testing.T, got string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("missing %q", line)
		}
	}
	if t.Failed() {
		t.Fatalf("metrics:\n%s", got)
	}
}

func TestMetricsStartAtZero(t *testing.T) {
	wantLines(t, scrape(t, NewMetrics("v9")),
		`transponder_build_info{version="v9"} 1`,
		`transponder_inflight_requests 0`,
		`transponder_unpriced_requests_total 0`,
		`# TYPE transponder_requests_total counter`,
		`# TYPE transponder_request_duration_seconds histogram`,
		`# TYPE transponder_limited_total counter`,
		`# TYPE transponder_events_dropped_total counter`,
		`# TYPE transponder_tokens_total counter`,
		`# TYPE transponder_cost_usd_total counter`,
		`# TYPE transponder_upstream_duration_seconds histogram`,
		`# TYPE transponder_fallbacks_total counter`,
	)
}

func TestMetricsRequestDone(t *testing.T) {
	m := NewMetrics("dev")
	m.Inflight(1)
	m.Inflight(1)
	m.Inflight(-1)
	m.RequestDone(Request{
		Endpoint: "chat.completions", Route: "fast", Provider: "oa", Model: "gpt-4o-mini", Code: 200,
		Duration:    300 * time.Millisecond,
		InputTokens: 1000, OutputTokens: 500, CacheReadTokens: 7, CacheWriteTokens: 3, ReasoningTokens: 11,
		CostNUSD: 450_000,
	})
	m.RequestDone(Request{Endpoint: "chat.completions", Route: "fast", Provider: "oa", Model: "gpt-4o-mini", Code: 502, Duration: 40 * time.Second})
	m.RequestDone(Request{Endpoint: "chat.completions", Code: 401, Duration: time.Millisecond})
	m.Limited("rate_limited")
	m.Limited("rate_limited")
	m.Limited("budget_exceeded")
	m.EventDropped("sql")
	m.Unpriced()
	m.Attempt("oa", "gpt-4o-mini", false, true, 2*time.Second)
	m.Fallback("fast", "oa", "claude", "server_error")

	wantLines(t, scrape(t, m),
		`transponder_inflight_requests 1`,
		`transponder_requests_total{endpoint="chat.completions",route="fast",provider="oa",model="gpt-4o-mini",code="200"} 1`,
		`transponder_requests_total{endpoint="chat.completions",route="fast",provider="oa",model="gpt-4o-mini",code="502"} 1`,
		`transponder_requests_total{endpoint="chat.completions",route="",provider="",model="",code="401"} 1`,
		`transponder_request_duration_seconds_bucket{endpoint="chat.completions",route="fast",le="0.25"} 0`,
		`transponder_request_duration_seconds_bucket{endpoint="chat.completions",route="fast",le="0.5"} 1`,
		`transponder_request_duration_seconds_bucket{endpoint="chat.completions",route="fast",le="60"} 2`,
		`transponder_request_duration_seconds_count{endpoint="chat.completions",route="fast"} 2`,
		`transponder_request_duration_seconds_sum{endpoint="chat.completions",route="fast"} 40.3`,
		`transponder_tokens_total{direction="input",provider="oa",model="gpt-4o-mini"} 1000`,
		`transponder_tokens_total{direction="output",provider="oa",model="gpt-4o-mini"} 500`,
		`transponder_tokens_total{direction="cache_read",provider="oa",model="gpt-4o-mini"} 7`,
		`transponder_tokens_total{direction="cache_write",provider="oa",model="gpt-4o-mini"} 3`,
		`transponder_tokens_total{direction="reasoning",provider="oa",model="gpt-4o-mini"} 11`,
		`transponder_cost_usd_total{provider="oa",model="gpt-4o-mini"} 0.00045`,
		`transponder_limited_total{reason="rate_limited"} 2`,
		`transponder_limited_total{reason="budget_exceeded"} 1`,
		`transponder_events_dropped_total{sink="sql"} 1`,
		`transponder_unpriced_requests_total 1`,
		`transponder_upstream_duration_seconds_bucket{provider="oa",model="gpt-4o-mini",le="2.5"} 1`,
		`transponder_upstream_duration_seconds_count{provider="oa",model="gpt-4o-mini"} 1`,
		`transponder_fallbacks_total{route="fast",from="oa",to="claude",reason="server_error"} 1`,
	)
}

// A model name the caller chose must not become a label until a provider has answered for
// it, and only so many of them may.
func TestMetricsCallerModelsAreBounded(t *testing.T) {
	m := NewMetrics("dev")
	call := func(model string, code int) {
		m.RequestDone(Request{Endpoint: "chat.completions", Route: "*", Provider: "p", Model: model, ModelFromCaller: true, Code: code, InputTokens: 1})
	}
	call("made-up", 404)
	call("made-up-2", 502)
	call("real", 200)
	call("real", 500) // known by now, so a later failure keeps its name
	for i := range maxCallerModels + 20 {
		call("m"+strconv.Itoa(i), 200)
	}
	m.Attempt("p", "never-answered", true, false, time.Second)

	got := scrape(t, m)
	wantLines(t, got,
		`transponder_requests_total{endpoint="chat.completions",route="*",provider="p",model="other",code="404"} 1`,
		`transponder_requests_total{endpoint="chat.completions",route="*",provider="p",model="other",code="502"} 1`,
		`transponder_requests_total{endpoint="chat.completions",route="*",provider="p",model="real",code="200"} 1`,
		`transponder_requests_total{endpoint="chat.completions",route="*",provider="p",model="real",code="500"} 1`,
		// "real" took one of the places, so 21 of the generated names did not get one.
		`transponder_requests_total{endpoint="chat.completions",route="*",provider="p",model="other",code="200"} 21`,
		`transponder_upstream_duration_seconds_count{provider="p",model="other"} 1`,
	)
	for _, name := range []string{"made-up", "never-answered", "m" + strconv.Itoa(maxCallerModels+10)} {
		if strings.Contains(got, `model="`+name+`"`) {
			t.Errorf("model %q became a label", name)
		}
	}
	if n := strings.Count(got, "transponder_requests_total{"); n > maxCallerModels+5 {
		t.Errorf("%d request series for %d allowed caller models", n, maxCallerModels)
	}
}

func TestNilMetrics(t *testing.T) {
	var m *Metrics
	m.Inflight(1)
	m.RequestDone(Request{Code: 200})
	m.Limited("x")
	m.EventDropped("sql")
	m.Unpriced()
	m.Attempt("p", "m", false, true, time.Second)
	m.Fallback("r", "a", "b", "error")
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
}
