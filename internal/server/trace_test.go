package server_test

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/telemetry"
)

const (
	callerTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpan  = "00f067aa0ba902b7"
)

// spanSink is an exporter that keeps the spans in memory.
type spanSink struct {
	mu    sync.Mutex
	spans []telemetry.SpanData
}

func (s *spanSink) Export(_ context.Context, spans []telemetry.SpanData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spans = append(s.spans, spans...)
	return nil
}

// traced is a gateway with tracing on. finish exports every span made so far and returns them.
type traced struct {
	*metered
	finish func() []telemetry.SpanData
}

func setupTraced(t *testing.T, routes []router.Route) *traced {
	t.Helper()
	sink := &spanSink{}
	tracer, err := telemetry.NewTracer(telemetry.TracerConfig{Exporter: sink})
	if err != nil {
		t.Fatal(err)
	}
	e := setupMetered(t, routes, func(d *server.Deps) { d.Tracer = tracer })
	finish := func() []telemetry.SpanData {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := tracer.Shutdown(ctx); err != nil {
			t.Fatal(err)
		}
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return append([]telemetry.SpanData(nil), sink.spans...)
	}
	t.Cleanup(func() { finish() })
	return &traced{metered: e, finish: finish}
}

// send posts body to path as the member key, with extra headers.
func (e *traced) send(path, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.keys[auth.RoleMember])
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

// tracedUpstream is a fake provider that remembers the trace headers it was sent.
type tracedUpstream struct {
	*counted
	mu          sync.Mutex
	traceparent string
	tracestate  string
}

func upstreamSeeingTrace(t *testing.T, code int, body string) *tracedUpstream {
	t.Helper()
	u := &tracedUpstream{}
	u.counted = countedUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.traceparent, u.tracestate = r.Header.Get("Traceparent"), r.Header.Get("Tracestate")
		u.mu.Unlock()
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	})
	return u
}

func (u *tracedUpstream) headers() (traceparent, tracestate string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.traceparent, u.tracestate
}

func spanNamed(t *testing.T, spans []telemetry.SpanData, name string) telemetry.SpanData {
	t.Helper()
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no span named %q among %d: %+v", name, len(spans), spans)
	return telemetry.SpanData{}
}

func wantAttrs(t *testing.T, s telemetry.SpanData, want map[string]any) {
	t.Helper()
	for key, w := range want {
		a, ok := s.Attr(key)
		if !ok {
			t.Errorf("span %q has no attribute %s", s.Name, key)
			continue
		}
		var got any
		switch a.Kind {
		case telemetry.AttrInt:
			got = a.Int
		case telemetry.AttrFloat:
			got = a.Float
		case telemetry.AttrBool:
			got = a.Bool
		default:
			got = a.Str
		}
		if got != w {
			t.Errorf("span %q: %s = %v (%T), want %v (%T)", s.Name, key, got, got, w, w)
		}
	}
}

func id16(b [16]byte) string { return hex.EncodeToString(b[:]) }
func id8(b [8]byte) string   { return hex.EncodeToString(b[:]) }

const zebraPrompt = `{"model":"fast","messages":[{"role":"user","content":"the zebra crossing prompt"}]}`

// One span for the call and one for each attempt, in the caller's trace, with the attempt's
// span id sent to the provider and nothing of the prompt or the answer in any of them.
func TestTraceOfACallThatFallsBack(t *testing.T) {
	// The failing provider quotes the prompt in its error, as some do.
	a := upstreamSeeingTrace(t, 500, `{"error":{"message":"could not process: the zebra crossing prompt"}}`)
	b := upstreamSeeingTrace(t, 200, okChat)
	e := setupTraced(t, route("fast", oaTarget("a", "gpt-5", a.counted), oaTarget("b", "gpt-4o-mini", b.counted)))
	rec := e.send("/v1/chat/completions", zebraPrompt, map[string]string{
		"Traceparent": "00-" + callerTrace + "-" + callerSpan + "-01",
		"Tracestate":  "vendor=abc",
	})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "from b") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	spans := e.finish()
	if len(spans) != 3 {
		t.Fatalf("%d spans, want 3: %+v", len(spans), spans)
	}
	call := spanNamed(t, spans, "POST /v1/chat/completions")
	first := spanNamed(t, spans, "chat gpt-5")
	second := spanNamed(t, spans, "chat gpt-4o-mini")

	for _, s := range spans {
		if id16(s.TraceID) != callerTrace || s.TraceState != "vendor=abc" {
			t.Errorf("span %q is not in the caller's trace: trace %s, state %q", s.Name, id16(s.TraceID), s.TraceState)
		}
	}
	if call.Kind != telemetry.SpanServer || id8(call.ParentID) != callerSpan || call.Failed {
		t.Errorf("call span: kind %d, parent %s, failed %v", call.Kind, id8(call.ParentID), call.Failed)
	}
	wantAttrs(t, call, map[string]any{
		"http.request.method":        "POST",
		"http.route":                 "/v1/chat/completions",
		"http.response.status_code":  int64(200),
		"transponder.endpoint":       "chat.completions",
		"transponder.route":          "fast",
		"transponder.provider":       "b",
		"transponder.model":          "gpt-4o-mini",
		"transponder.fallbacks":      int64(1),
		"gen_ai.operation.name":      "chat",
		"gen_ai.usage.input_tokens":  int64(1000),
		"gen_ai.usage.output_tokens": int64(500),
		"transponder.cost_usd":       0.00045,
		"transponder.request_id":     rec.Header().Get("X-Request-ID"),
	})

	if first.Kind != telemetry.SpanClient || first.ParentID != call.SpanID || !first.Failed || first.Message != "server_error" {
		t.Errorf("first attempt: kind %d, parent %s, failed %v, message %q", first.Kind, id8(first.ParentID), first.Failed, first.Message)
	}
	wantAttrs(t, first, map[string]any{
		"gen_ai.operation.name":     "chat",
		"gen_ai.provider.name":      "openai",
		"gen_ai.request.model":      "gpt-5",
		"transponder.provider":      "a",
		"transponder.route":         "fast",
		"transponder.attempt":       int64(0),
		"http.response.status_code": int64(500),
		"error.type":                "server_error",
	})
	if _, has := first.Attr("gen_ai.usage.input_tokens"); has {
		t.Error("a failed attempt carries token counts")
	}

	if second.Kind != telemetry.SpanClient || second.ParentID != call.SpanID || second.Failed {
		t.Errorf("second attempt: kind %d, parent %s, failed %v", second.Kind, id8(second.ParentID), second.Failed)
	}
	wantAttrs(t, second, map[string]any{
		"gen_ai.provider.name":       "openai",
		"gen_ai.request.model":       "gpt-4o-mini",
		"transponder.provider":       "b",
		"transponder.attempt":        int64(1),
		"http.response.status_code":  int64(200),
		"gen_ai.usage.input_tokens":  int64(1000),
		"gen_ai.usage.output_tokens": int64(500),
		"transponder.cost_usd":       0.00045,
	})
	if first.Start.Before(call.Start) || second.Start.Before(first.End) || call.End.Before(second.End) {
		t.Error("the attempts do not sit inside the call, one after the other")
	}

	// Each provider was told the span of its own attempt, in the caller's trace, and got
	// tracestate as the caller sent it.
	for _, tt := range []struct {
		up   *tracedUpstream
		span telemetry.SpanData
	}{{a, first}, {b, second}} {
		traceparent, tracestate := tt.up.headers()
		if want := "00-" + callerTrace + "-" + id8(tt.span.SpanID) + "-01"; traceparent != want || tracestate != "vendor=abc" {
			t.Errorf("attempt %q sent traceparent %q and tracestate %q, want %q and vendor=abc", tt.span.Name, traceparent, tracestate, want)
		}
	}

	dump := fmt.Sprintf("%+v", spans)
	for _, text := range []string{"zebra", "from b", "could not process"} {
		if strings.Contains(dump, text) {
			t.Errorf("the spans contain %q, which is prompt, answer or provider error text:\n%s", text, dump)
		}
	}
}

func TestTraceStartsAtTheGatewayWithoutTraceparent(t *testing.T) {
	up := upstreamSeeingTrace(t, 200, okChat)
	e := setupTraced(t, route("fast", oaTarget("a", "gpt-4o-mini", up.counted)))
	if rec := e.send("/v1/chat/completions", zebraPrompt, nil); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	spans := e.finish()
	if len(spans) != 2 {
		t.Fatalf("%d spans, want 2", len(spans))
	}
	call, attempt := spanNamed(t, spans, "POST /v1/chat/completions"), spanNamed(t, spans, "chat gpt-4o-mini")
	if call.ParentID != [8]byte{} || call.TraceID == [16]byte{} || attempt.TraceID != call.TraceID || attempt.ParentID != call.SpanID {
		t.Fatalf("call %+v, attempt %+v", call, attempt)
	}
	traceparent, tracestate := up.headers()
	if want := "00-" + id16(call.TraceID) + "-" + id8(attempt.SpanID) + "-01"; traceparent != want || tracestate != "" {
		t.Fatalf("the provider got traceparent %q and tracestate %q, want %q and none", traceparent, tracestate, want)
	}
}

// A caller that did not sample its trace gets no spans, and the provider is told so in the
// same trace.
func TestUnsampledCallerIsNotTraced(t *testing.T) {
	up := upstreamSeeingTrace(t, 200, okChat)
	e := setupTraced(t, route("fast", oaTarget("a", "gpt-4o-mini", up.counted)))
	rec := e.send("/v1/chat/completions", zebraPrompt, map[string]string{
		"Traceparent": "00-" + callerTrace + "-" + callerSpan + "-00",
		"Tracestate":  "vendor=abc",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if spans := e.finish(); len(spans) != 0 {
		t.Fatalf("%d spans exported for an unsampled trace: %+v", len(spans), spans)
	}
	traceparent, tracestate := up.headers()
	if !strings.HasPrefix(traceparent, "00-"+callerTrace+"-") || !strings.HasSuffix(traceparent, "-00") || tracestate != "vendor=abc" {
		t.Fatalf("the provider got traceparent %q and tracestate %q", traceparent, tracestate)
	}
}

// With tracing off the caller's headers go to the provider exactly as they came.
func TestTraceHeadersPassThroughWithTracingOff(t *testing.T) {
	up := upstreamSeeingTrace(t, 200, okChat)
	e := setupMetered(t, route("fast", oaTarget("a", "gpt-4o-mini", up.counted)), nil)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(zebraPrompt))
	req.Header.Set("Authorization", "Bearer "+e.keys[auth.RoleMember])
	req.Header.Set("Traceparent", "00-"+callerTrace+"-"+callerSpan+"-01")
	req.Header.Set("Tracestate", "vendor=abc")
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	traceparent, tracestate := up.headers()
	if traceparent != "00-"+callerTrace+"-"+callerSpan+"-01" || tracestate != "vendor=abc" {
		t.Fatalf("the provider got traceparent %q and tracestate %q", traceparent, tracestate)
	}
}

func TestTraceOfRequestsThatReachNoProvider(t *testing.T) {
	down := upstreamSeeingTrace(t, 503, "down")
	e := setupTraced(t, route("fast", oaTarget("a", "gpt-5", down.counted)))

	if rec := e.send("/v1/chat/completions", `{"model":"no-such-model","messages":[]}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model: status %d", rec.Code)
	}
	if rec := e.send("/v1/chat/completions", zebraPrompt, nil); rec.Code != http.StatusBadGateway {
		t.Fatalf("provider down: status %d", rec.Code)
	}
	spans := e.finish()
	// The 404 on its own, and the failed call with its one attempt.
	if len(spans) != 3 {
		t.Fatalf("%d spans, want 3: %+v", len(spans), spans)
	}
	var refused, failed telemetry.SpanData
	for _, s := range spans {
		if s.Name != "POST /v1/chat/completions" {
			continue
		}
		if a, _ := s.Attr("http.response.status_code"); a.Int == http.StatusNotFound {
			refused = s
		} else {
			failed = s
		}
	}
	// A 404 is the caller's mistake, not an error of the gateway.
	if refused.Failed {
		t.Error("the 404 is marked as an error")
	}
	wantAttrs(t, refused, map[string]any{"transponder.endpoint": "chat.completions", "http.response.status_code": int64(404)})
	for _, key := range []string{"transponder.provider", "transponder.route", "gen_ai.usage.input_tokens"} {
		if _, has := refused.Attr(key); has {
			t.Errorf("the 404 span has %s", key)
		}
	}
	if !failed.Failed {
		t.Error("the 502 is not marked as an error")
	}
	wantAttrs(t, failed, map[string]any{"http.response.status_code": int64(502), "error.type": "502", "transponder.provider": "a"})
	attempt := spanNamed(t, spans, "chat gpt-5")
	wantAttrs(t, attempt, map[string]any{"error.type": "server_error", "http.response.status_code": int64(503)})
}

// Someone with no key cannot make the gateway export spans, even by sending a traceparent
// that says to sample. Probes and scrapes are not traced either.
func TestOnlyKnownCallersAreTraced(t *testing.T) {
	e := setupTraced(t, echoRoutes(echo.New("echo")))
	sampled := "00-" + callerTrace + "-" + callerSpan + "-01"
	for _, tt := range []struct {
		method, path, key string
		want              int
	}{
		{http.MethodPost, "/v1/chat/completions", "", http.StatusUnauthorized},
		{http.MethodPost, "/v1/chat/completions", "tp_not_a_key", http.StatusUnauthorized},
		{http.MethodGet, "/nowhere", "", http.StatusNotFound},
		{http.MethodGet, "/healthz", "", http.StatusOK},
		{http.MethodGet, "/readyz", "", http.StatusOK},
		{http.MethodGet, "/metrics", e.keys[auth.RoleViewer], http.StatusOK},
	} {
		req := httptest.NewRequestWithContext(context.Background(), tt.method, tt.path, strings.NewReader(ping))
		req.Header.Set("Traceparent", sampled)
		if tt.key != "" {
			req.Header.Set("Authorization", "Bearer "+tt.key)
		}
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != tt.want {
			t.Fatalf("%s %s: status %d, want %d", tt.method, tt.path, rec.Code, tt.want)
		}
	}
	if spans := e.finish(); len(spans) != 0 {
		t.Fatalf("%d spans exported: %+v", len(spans), spans)
	}
}

// A tracestate longer than the standard allows is not copied onto the spans, where it
// would be queued once per span. The provider still gets the header as the caller sent it.
func TestOversizedTracestateStaysOffTheSpans(t *testing.T) {
	up := upstreamSeeingTrace(t, 200, okChat)
	e := setupTraced(t, route("fast", oaTarget("a", "gpt-4o-mini", up.counted)))
	huge := "vendor=" + strings.Repeat("x", 4000)
	rec := e.send("/v1/chat/completions", zebraPrompt, map[string]string{
		"Traceparent": "00-" + callerTrace + "-" + callerSpan + "-01",
		"Tracestate":  huge,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	spans := e.finish()
	if len(spans) != 2 {
		t.Fatalf("%d spans, want 2", len(spans))
	}
	for _, s := range spans {
		if s.TraceState != "" || id16(s.TraceID) != callerTrace {
			t.Errorf("span %q: tracestate of %d bytes, trace %s", s.Name, len(s.TraceState), id16(s.TraceID))
		}
	}
	if _, tracestate := up.headers(); tracestate != huge {
		t.Errorf("the provider got a tracestate of %d bytes, want the caller's %d", len(tracestate), len(huge))
	}
}

func TestTraceOfAStream(t *testing.T) {
	e := setupTraced(t, echoRoutes(echo.New("echo")))
	rec := e.send("/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"count these words please"}]}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	recs, err := e.db.RecentUsage(context.Background(), store.DefaultOrg, 10)
	if err != nil || len(recs) != 1 {
		t.Fatalf("usage records %+v, err %v", recs, err)
	}
	spans := e.finish()
	attempt := spanNamed(t, spans, "chat m")
	if attempt.Failed {
		t.Fatal("the attempt is marked as an error")
	}
	// The attempt's span ends with the stream, so it has the stream's token counts.
	wantAttrs(t, attempt, map[string]any{
		"gen_ai.provider.name":       "echo",
		"gen_ai.usage.input_tokens":  recs[0].InputTokens,
		"gen_ai.usage.output_tokens": recs[0].OutputTokens,
	})
	if strings.Contains(fmt.Sprintf("%+v", spans), "count these words") {
		t.Fatal("the spans contain the prompt")
	}
}

// A target that cannot take the request gets a span that says so, and no upstream call.
func TestTraceOfASkippedTarget(t *testing.T) {
	a := upstreamSeeingTrace(t, 200, "{}")
	b := upstreamSeeingTrace(t, 200, `{"object":"list","data":[{"index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":3}}`)
	e := setupTraced(t, route("embed", anTarget("claude-sonnet-5-5", a.counted), oaTarget("b", "text-embedding-3-small", b.counted)))
	if rec := e.send("/v1/embeddings", `{"model":"embed","input":"hello"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	spans := e.finish()
	if len(spans) != 3 || a.calls.Load() != 0 {
		t.Fatalf("%d spans, the skipped provider was called %d times", len(spans), a.calls.Load())
	}
	skipped := spanNamed(t, spans, "embeddings claude-sonnet-5-5")
	if !skipped.Failed {
		t.Error("the skipped target's span is not marked as an error")
	}
	wantAttrs(t, skipped, map[string]any{"error.type": "unsupported", "gen_ai.provider.name": "anthropic", "transponder.attempt": int64(0)})
	answered := spanNamed(t, spans, "embeddings text-embedding-3-small")
	wantAttrs(t, answered, map[string]any{"gen_ai.operation.name": "embeddings", "gen_ai.usage.input_tokens": int64(3), "transponder.attempt": int64(1)})
	wantAttrs(t, spanNamed(t, spans, "POST /v1/embeddings"), map[string]any{"transponder.fallbacks": int64(1), "transponder.provider": "b"})
}
