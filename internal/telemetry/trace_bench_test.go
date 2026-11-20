package telemetry

import (
	"context"
	"testing"
)

type discard struct{}

func (discard) Export(context.Context, []SpanData) error { return nil }

// spansOfOneCall does what the gateway does for a call with one attempt.
func spansOfOneCall(tr *Tracer) {
	call := tr.Start("POST", SpanServer, "", "")
	attempt := call.Child("chat gpt-4o-mini", SpanClient)
	attempt.SetString("gen_ai.provider.name", "openai")
	attempt.SetString("gen_ai.request.model", "gpt-4o-mini")
	attempt.SetInt("gen_ai.usage.input_tokens", 1000)
	attempt.SetInt("gen_ai.usage.output_tokens", 500)
	attempt.SetFloat("transponder.cost_usd", 0.00045)
	_ = attempt.Traceparent()
	attempt.End()
	call.SetName("POST /v1/chat/completions")
	call.SetInt("http.response.status_code", 200)
	call.SetString("transponder.route", "fast")
	call.End()
}

// With no otel.endpoint the tracer is nil.
func BenchmarkSpansOff(b *testing.B) {
	var tr *Tracer
	b.ReportAllocs()
	for b.Loop() {
		spansOfOneCall(tr)
	}
}

func BenchmarkSpansOn(b *testing.B) {
	tr, err := NewTracer(TracerConfig{Exporter: discard{}})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = tr.Shutdown(context.Background()) }()
	b.ReportAllocs()
	for b.Loop() {
		spansOfOneCall(tr)
	}
}

// A call whose caller did not sample the trace: ids are made and passed on, nothing is kept.
func BenchmarkSpansUnsampled(b *testing.B) {
	tr, err := NewTracer(TracerConfig{Exporter: discard{}})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = tr.Shutdown(context.Background()) }()
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"
	b.ReportAllocs()
	for b.Loop() {
		call := tr.Start("POST", SpanServer, parent, "")
		attempt := call.Child("chat gpt-4o-mini", SpanClient)
		attempt.SetString("gen_ai.provider.name", "openai")
		_ = attempt.Traceparent()
		attempt.End()
		call.End()
	}
}
