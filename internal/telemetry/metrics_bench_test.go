package telemetry

import (
	"io"
	"testing"
	"time"
)

// What one request adds: the in-flight gauge twice, the request with its tokens and cost,
// and one attempt.
func BenchmarkMetricsPerRequest(b *testing.B) {
	m := NewMetrics("bench")
	req := Request{
		Endpoint: "chat.completions", Route: "fast", Provider: "oa", Model: "gpt-4o-mini", Code: 200,
		Duration: 300 * time.Millisecond, InputTokens: 1000, OutputTokens: 500, CostNUSD: 450_000,
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			m.Inflight(1)
			m.Attempt("oa", "gpt-4o-mini", false, true, 250*time.Millisecond)
			m.RequestDone(req)
			m.Inflight(-1)
		}
	})
}

func BenchmarkMetricsScrape(b *testing.B) {
	m := NewMetrics("bench")
	for _, model := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		for _, code := range []int{200, 400, 429, 502} {
			m.RequestDone(Request{Endpoint: "chat.completions", Route: "fast", Provider: "oa", Model: model, Code: code, InputTokens: 1, OutputTokens: 1, CostNUSD: 1})
			m.Attempt("oa", model, false, true, time.Second)
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		_, _ = m.reg.WriteTo(io.Discard)
	}
}
