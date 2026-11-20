package telemetry

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// sink is an exporter that keeps what it is given.
type sink struct {
	mu    sync.Mutex
	spans []SpanData
	err   error
	// entered, when set, gets a value at the start of each export, and release holds it there.
	entered chan struct{}
	release chan struct{}
	// finished, when set, gets a value once an export has its result.
	finished chan struct{}
}

func (s *sink) Export(_ context.Context, spans []SpanData) error {
	if s.entered != nil {
		s.entered <- struct{}{}
		<-s.release
	}
	err := s.keep(spans)
	if s.finished != nil {
		s.finished <- struct{}{}
	}
	return err
}

func (s *sink) keep(spans []SpanData) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.spans = append(s.spans, spans...)
	return nil
}

func (s *sink) all() []SpanData {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SpanData(nil), s.spans...)
}

func (s *sink) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func newTracer(t *testing.T, cfg TracerConfig) *Tracer {
	t.Helper()
	tr, err := NewTracer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Shutdown(context.Background()) })
	return tr
}

func shutdown(t *testing.T, tr *Tracer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tr.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

const (
	traceID  = "4bf92f3577b34da6a3ce929d0e0e4736"
	parentID = "00f067aa0ba902b7"
)

func TestParseTraceparent(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		ok      bool
		sampled bool
	}{
		{"sampled", "00-" + traceID + "-" + parentID + "-01", true, true},
		{"not sampled", "00-" + traceID + "-" + parentID + "-00", true, false},
		{"other flags set", "00-" + traceID + "-" + parentID + "-03", true, true},
		{"a later version with more fields", "01-" + traceID + "-" + parentID + "-01-extra", true, true},
		{"empty", "", false, false},
		{"too short", "00-" + traceID + "-" + parentID, false, false},
		{"version 00 with more fields", "00-" + traceID + "-" + parentID + "-01-extra", false, false},
		{"a later version with junk after it", "01-" + traceID + "-" + parentID + "-01extra", false, false},
		{"version ff", "ff-" + traceID + "-" + parentID + "-01", false, false},
		{"uppercase", "00-" + strings.ToUpper(traceID) + "-" + parentID + "-01", false, false},
		{"not hex", "00-" + strings.Repeat("z", 32) + "-" + parentID + "-01", false, false},
		{"zero trace id", "00-" + strings.Repeat("0", 32) + "-" + parentID + "-01", false, false},
		{"zero span id", "00-" + traceID + "-" + strings.Repeat("0", 16) + "-01", false, false},
		{"dashes in the wrong place", "00" + traceID + "--" + parentID + "-01", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc, ok := ParseTraceparent(tt.header)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if hex.EncodeToString(sc.TraceID[:]) != traceID || hex.EncodeToString(sc.SpanID[:]) != parentID || sc.Sampled != tt.sampled {
				t.Fatalf("parsed %+v", sc)
			}
			want := "00-" + traceID + "-" + parentID + "-00"
			if tt.sampled {
				want = "00-" + traceID + "-" + parentID + "-01"
			}
			if got := sc.Traceparent(); got != want {
				t.Fatalf("Traceparent() = %q, want %q", got, want)
			}
		})
	}
}

func TestSpansOfOneTrace(t *testing.T) {
	out := &sink{}
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tr := newTracer(t, TracerConfig{Exporter: out, Now: func() time.Time { now = now.Add(time.Second); return now }})

	root := tr.Start("POST /v1/chat/completions", SpanServer, "", "")
	child := root.Child("chat gpt-4o-mini", SpanClient)
	child.SetString("gen_ai.provider.name", "openai")
	child.SetString("left.out", "")
	child.SetInt("gen_ai.usage.input_tokens", 1000)
	child.SetFloat("transponder.cost_usd", 0.00045)
	child.SetBool("transponder.usage.estimated", true)
	child.Fail("server_error")
	child.End()
	child.End() // a second End adds nothing
	root.SetName("renamed")
	root.End()
	shutdown(t, tr)

	spans := out.all()
	if len(spans) != 2 {
		t.Fatalf("%d spans exported, want 2: %+v", len(spans), spans)
	}
	c, r := spans[0], spans[1]
	if r.Name != "renamed" || r.Kind != SpanServer || r.ParentID != [8]byte{} || r.Failed {
		t.Fatalf("root span %+v", r)
	}
	if c.Name != "chat gpt-4o-mini" || c.Kind != SpanClient || c.TraceID != r.TraceID || c.ParentID != r.SpanID || c.SpanID == r.SpanID {
		t.Fatalf("child span %+v under root %+v", c, r)
	}
	if !c.Failed || c.Message != "server_error" {
		t.Fatalf("child status: failed %v, message %q", c.Failed, c.Message)
	}
	if !c.End.After(c.Start) || !r.End.After(r.Start) || c.Start.Before(r.Start) {
		t.Fatalf("times: root %v to %v, child %v to %v", r.Start, r.End, c.Start, c.End)
	}
	want := []Attr{
		{Key: "gen_ai.provider.name", Kind: AttrString, Str: "openai"},
		{Key: "gen_ai.usage.input_tokens", Kind: AttrInt, Int: 1000},
		{Key: "transponder.cost_usd", Kind: AttrFloat, Float: 0.00045},
		{Key: "transponder.usage.estimated", Kind: AttrBool, Bool: true},
	}
	if len(c.Attrs) != len(want) {
		t.Fatalf("attributes %+v, want %+v", c.Attrs, want)
	}
	for _, w := range want {
		if got, ok := c.Attr(w.Key); !ok || got != w {
			t.Errorf("attribute %s = %+v, want %+v", w.Key, got, w)
		}
	}
	if _, ok := c.Attr("left.out"); ok {
		t.Error("an empty string attribute was kept")
	}
	if root.Traceparent() != "00-"+hex.EncodeToString(r.TraceID[:])+"-"+hex.EncodeToString(r.SpanID[:])+"-01" {
		t.Fatalf("root traceparent %q does not match its span", root.Traceparent())
	}
}

func TestSpanJoinsTheCallersTrace(t *testing.T) {
	out := &sink{}
	tr := newTracer(t, TracerConfig{Exporter: out})
	s := tr.Start("x", SpanServer, "00-"+traceID+"-"+parentID+"-01", "vendor=abc")
	child := s.Child("y", SpanClient)
	child.End()
	s.End()
	shutdown(t, tr)
	spans := out.all()
	if len(spans) != 2 {
		t.Fatalf("%d spans, want 2", len(spans))
	}
	for _, sp := range spans {
		if hex.EncodeToString(sp.TraceID[:]) != traceID || sp.TraceState != "vendor=abc" {
			t.Fatalf("span left the caller's trace: %+v", sp)
		}
	}
	if hex.EncodeToString(spans[1].ParentID[:]) != parentID {
		t.Fatalf("the first span's parent is %x, want the caller's span", spans[1].ParentID)
	}
	if !strings.HasPrefix(child.Traceparent(), "00-"+traceID+"-") || !strings.HasSuffix(child.Traceparent(), "-01") {
		t.Fatalf("child traceparent %q", child.Traceparent())
	}
}

// A caller that did not sample its trace gets no spans from us, but the trace id and the
// decision still travel on.
func TestUnsampledParentIsNotRecorded(t *testing.T) {
	out := &sink{}
	tr := newTracer(t, TracerConfig{Exporter: out})
	s := tr.Start("x", SpanServer, "00-"+traceID+"-"+parentID+"-00", "")
	s.SetString("k", "v")
	s.Fail("nope")
	child := s.Child("y", SpanClient)
	child.SetInt("n", 1)
	child.End()
	s.End()
	shutdown(t, tr)
	if n := len(out.all()); n != 0 {
		t.Fatalf("%d spans exported for an unsampled trace", n)
	}
	if got := child.Traceparent(); !strings.HasPrefix(got, "00-"+traceID+"-") || !strings.HasSuffix(got, "-00") || strings.Contains(got, parentID) {
		t.Fatalf("child traceparent %q", got)
	}
	if sc := s.Context(); sc.Sampled || !sc.Valid() {
		t.Fatalf("context %+v", sc)
	}
}

func TestSampleRatio(t *testing.T) {
	out := &sink{}
	// So small that a trace starting here is never picked.
	tr := newTracer(t, TracerConfig{Exporter: out, SampleRatio: 1e-18})
	for range 200 {
		tr.Start("x", SpanServer, "", "").End()
	}
	// The caller's own decision still wins.
	tr.Start("x", SpanServer, "00-"+traceID+"-"+parentID+"-01", "").End()
	shutdown(t, tr)
	if n := len(out.all()); n != 1 {
		t.Fatalf("%d spans exported, want only the one the caller sampled", n)
	}
}

func TestNilTracerAndSpan(t *testing.T) {
	var tr *Tracer
	s := tr.Start("x", SpanServer, "00-"+traceID+"-"+parentID+"-01", "")
	if s != nil {
		t.Fatal("a nil tracer made a span")
	}
	c := s.Child("y", SpanClient)
	c.SetName("z")
	c.SetString("k", "v")
	c.SetInt("k", 1)
	c.SetFloat("k", 1)
	c.SetBool("k", true)
	c.Fail("x")
	c.End()
	if c != nil || c.Traceparent() != "" || c.Context().Valid() {
		t.Fatal("a nil span has a context")
	}
	if err := tr.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBoundedState(t *testing.T) {
	atLimit := "k=" + strings.Repeat("v", maxTraceState-2)
	for in, want := range map[string]string{
		"":                        "",
		"vendor=abc,other=1":      "vendor=abc,other=1",
		atLimit:                   atLimit,
		atLimit + "v":             "",
		"vendor=a\r\nInjected: 1": "",
		"vendor=café":             "",
	} {
		if got := boundedState(in); got != want {
			t.Errorf("boundedState(%d bytes %q...) = %d bytes, want %d", len(in), in[:min(len(in), 20)], len(got), len(want))
		}
	}
}

// What is logged about the collector must not include a key written into its URL.
func TestCollectorHost(t *testing.T) {
	for in, want := range map[string]string{ //nolint:gosec // made-up keys, to check they are not logged
		"http://collector:4318":                           "collector:4318",
		"https://user:secret@otlp.example.com/otlp":       "otlp.example.com",
		"https://otlp.example.com/v1/traces?api_key=s3cr": "otlp.example.com",
		"://": "",
	} {
		if got := CollectorHost(in); got != want {
			t.Errorf("CollectorHost(%q) = %q, want %q", in, got, want)
		}
	}
}

// An endpoint that is refused is not repeated in the error, since it can hold a key.
func TestBadEndpointIsNotEchoed(t *testing.T) {
	_, err := NewTracer(TracerConfig{Endpoint: "grpc://user:hunter2@collector:4317"}) //nolint:gosec // a made-up key
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "otel.endpoint") {
		t.Fatalf("err = %v", err)
	}
}

func TestNewTracer(t *testing.T) {
	if tr, err := NewTracer(TracerConfig{}); tr != nil || err != nil {
		t.Fatalf("with no endpoint: %v, %v; want no tracer and no error", tr, err)
	}
	for name, cfg := range map[string]TracerConfig{
		"grpc address":   {Endpoint: "collector:4317"},
		"no host":        {Endpoint: "http://"},
		"another scheme": {Endpoint: "grpc://collector:4317"},
		"ratio above 1":  {Endpoint: "http://collector:4318", SampleRatio: 1.5},
		"negative ratio": {Endpoint: "http://collector:4318", SampleRatio: -0.1},
	} {
		t.Run(name, func(t *testing.T) {
			if tr, err := NewTracer(cfg); err == nil {
				_ = tr.Shutdown(context.Background())
				t.Fatal("accepted")
			}
		})
	}
}

func TestTracesURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://collector:4318":           "http://collector:4318/v1/traces",
		"http://collector:4318/":          "http://collector:4318/v1/traces",
		"https://otlp.example.com/otlp":   "https://otlp.example.com/otlp/v1/traces",
		"http://collector:4318/v1/traces": "http://collector:4318/v1/traces",
		"https://otlp.example.com/x/?a=b": "https://otlp.example.com/x/v1/traces?a=b",
	} {
		got, err := tracesURL(in)
		if err != nil || got != want {
			t.Errorf("tracesURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestSpansAreExportedOnTheTick(t *testing.T) {
	out := &sink{entered: make(chan struct{}, 1), release: make(chan struct{})}
	close(out.release)
	tr := newTracer(t, TracerConfig{Exporter: out, FlushEvery: time.Millisecond})
	tr.Start("x", SpanServer, "", "").End()
	select {
	case <-out.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the span was not exported without a shutdown")
	}
}

// A collector that is slow must not block requests: once the queue is full, spans are
// dropped and counted.
func TestFullQueueDropsSpans(t *testing.T) {
	out := &sink{entered: make(chan struct{}), release: make(chan struct{})}
	var mu sync.Mutex
	dropped := 0
	tr := newTracer(t, TracerConfig{Exporter: out, FlushEvery: time.Hour, Dropped: func(n int) {
		mu.Lock()
		dropped += n
		mu.Unlock()
	}})
	end := func(n int) {
		for range n {
			tr.Start("x", SpanServer, "", "").End()
		}
	}
	// A full batch starts an export, which the sink holds open.
	end(spanBatchSize)
	select {
	case <-out.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("a full batch was not exported")
	}
	end(spanQueueSize)
	end(7)
	mu.Lock()
	got := dropped
	mu.Unlock()
	if got != 7 {
		t.Fatalf("%d spans dropped, want the 7 past the full queue", got)
	}
	close(out.release)
	go func() {
		for range out.entered { //nolint:revive // drain the later exports
		}
	}()
	shutdown(t, tr)
	close(out.entered)
	if n := len(out.all()); n != spanBatchSize+spanQueueSize {
		t.Fatalf("%d spans exported, want %d", n, spanBatchSize+spanQueueSize)
	}
}

func TestFailedExportDropsTheBatchAndLogsOnce(t *testing.T) {
	out := &sink{finished: make(chan struct{}, 1)}
	out.fail(errors.New("the collector answered 503 Service Unavailable"))
	var logs bytes.Buffer
	var mu sync.Mutex
	dropped := 0
	tr := newTracer(t, TracerConfig{
		Exporter: out, FlushEvery: time.Millisecond, Logger: slog.New(slog.NewTextHandler(&lockedWriter{w: &logs, mu: &mu}, nil)),
		Dropped: func(n int) {
			mu.Lock()
			dropped += n
			mu.Unlock()
		},
	})
	export := func() {
		t.Helper()
		tr.Start("x", SpanServer, "", "").End()
		select {
		case <-out.finished:
		case <-time.After(10 * time.Second):
			t.Fatal("no export")
		}
	}
	export()
	export()
	out.fail(nil)
	export()
	shutdown(t, tr)

	mu.Lock()
	defer mu.Unlock()
	if dropped != 2 {
		t.Fatalf("%d spans dropped, want the 2 the collector refused", dropped)
	}
	if n := strings.Count(logs.String(), "exporting spans failed"); n != 1 {
		t.Fatalf("the failure was logged %d times, want once:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "exporting spans works again") {
		t.Fatalf("the recovery was not logged:\n%s", logs.String())
	}
	if n := len(out.all()); n != 1 {
		t.Fatalf("%d spans kept by the collector, want 1", n)
	}
}

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestShutdownTimesOut(t *testing.T) {
	out := &sink{entered: make(chan struct{}, 1), release: make(chan struct{})}
	tr, err := NewTracer(TracerConfig{Exporter: out})
	if err != nil {
		t.Fatal(err)
	}
	tr.Start("x", SpanServer, "", "").End()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := tr.Shutdown(ctx); err == nil {
		t.Fatal("Shutdown returned no error with the exporter stuck and the context done")
	}
	close(out.release)
	shutdown(t, tr)
}

// The collector is an httptest server that reads what a real OTLP/HTTP collector would.
func TestOTLPExport(t *testing.T) {
	type received struct {
		path, contentType, auth string
		body                    []byte
	}
	got := make(chan received, 1)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- received{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"partialSuccess":{}}`)
	}))
	defer collector.Close()

	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	now := start
	tr := newTracer(t, TracerConfig{
		Endpoint: collector.URL, Headers: map[string]string{"Authorization": "Bearer otel-key"}, Version: "v0.3.0",
		Client: collector.Client(), Now: func() time.Time { now = now.Add(time.Second); return now },
	})
	root := tr.Start("POST /v1/chat/completions", SpanServer, "00-"+traceID+"-"+parentID+"-01", "vendor=abc")
	root.SetInt("http.response.status_code", 502)
	root.SetString("transponder.route", "fast")
	root.SetFloat("transponder.cost_usd", 0.5)
	root.SetBool("transponder.usage.estimated", true)
	root.Fail("")
	child := root.Child("chat gpt-5", SpanClient)
	child.Fail("server_error")
	child.End()
	root.End()
	shutdown(t, tr)

	var req received
	select {
	case req = <-got:
	case <-time.After(10 * time.Second):
		t.Fatal("the collector got nothing")
	}
	if req.path != "/v1/traces" || req.contentType != "application/json" || req.auth != "Bearer otel-key" {
		t.Fatalf("request: path %q, content type %q, authorization %q", req.path, req.contentType, req.auth)
	}
	var body struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []map[string]any `json:"attributes"`
			} `json:"resource"`
			ScopeSpans []struct {
				Scope map[string]string `json:"scope"`
				Spans []map[string]any  `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatalf("decoding %s: %v", req.body, err)
	}
	if len(body.ResourceSpans) != 1 || len(body.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("body %s", req.body)
	}
	resource, _ := json.Marshal(body.ResourceSpans[0].Resource.Attributes)
	wantResource := `[{"key":"service.name","value":{"stringValue":"transponder"}},{"key":"service.version","value":{"stringValue":"v0.3.0"}}]`
	if string(resource) != wantResource {
		t.Fatalf("resource %s, want %s", resource, wantResource)
	}
	scope := body.ResourceSpans[0].ScopeSpans[0]
	if scope.Scope["name"] != scopeName || scope.Scope["version"] != "v0.3.0" || len(scope.Spans) != 2 {
		t.Fatalf("scope %+v with %d spans", scope.Scope, len(scope.Spans))
	}
	c, r := scope.Spans[0], scope.Spans[1]
	rootID := r["spanId"].(string)
	wantRoot := map[string]any{
		"traceId": traceID, "spanId": rootID, "parentSpanId": parentID, "traceState": "vendor=abc",
		"name": "POST /v1/chat/completions", "kind": float64(2),
		// One second per reading of the clock: the root starts first and ends last.
		"startTimeUnixNano": strconv.FormatInt(start.Add(time.Second).UnixNano(), 10),
		"endTimeUnixNano":   strconv.FormatInt(start.Add(4*time.Second).UnixNano(), 10),
		"attributes": []any{
			map[string]any{"key": "http.response.status_code", "value": map[string]any{"intValue": "502"}},
			map[string]any{"key": "transponder.route", "value": map[string]any{"stringValue": "fast"}},
			map[string]any{"key": "transponder.cost_usd", "value": map[string]any{"doubleValue": 0.5}},
			map[string]any{"key": "transponder.usage.estimated", "value": map[string]any{"boolValue": true}},
		},
		"status": map[string]any{"code": float64(2)},
	}
	if gotJSON, wantJSON := mustJSON(t, r), mustJSON(t, wantRoot); gotJSON != wantJSON {
		t.Fatalf("root span\n got %s\nwant %s", gotJSON, wantJSON)
	}
	if len(rootID) != 16 || rootID == parentID {
		t.Fatalf("root span id %q", rootID)
	}
	if c["traceId"] != traceID || c["parentSpanId"] != rootID || c["kind"] != float64(3) || c["name"] != "chat gpt-5" {
		t.Fatalf("child span %v", c)
	}
	if status := mustJSON(t, c["status"]); status != `{"code":2,"message":"server_error"}` {
		t.Fatalf("child status %s", status)
	}
	if _, has := c["attributes"]; has {
		t.Fatalf("a span with no attributes sent the field: %v", c)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestOTLPExportReportsARefusal(t *testing.T) {
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer collector.Close()
	e := &otlpExporter{url: collector.URL + "/v1/traces", client: collector.Client()}
	err := e.Export(context.Background(), []SpanData{{Name: "x"}})
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want it to name the status", err)
	}
	collector.Close()
	if err := e.Export(context.Background(), []SpanData{{Name: "x"}}); err == nil {
		t.Fatal("exporting to a collector that is gone returned no error")
	}
}
