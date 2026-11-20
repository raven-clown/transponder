package telemetry

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Batching follows the defaults of the OpenTelemetry batch span processor.
const (
	spanQueueSize     = 2048
	spanBatchSize     = 512
	spanFlushEvery    = 5 * time.Second
	spanExportTimeout = 10 * time.Second
)

// SpanKind says which side of a call a span is.
type SpanKind int

// The values are the ones OTLP uses.
const (
	SpanServer SpanKind = 2
	SpanClient SpanKind = 3
)

// SpanContext is the part of a span that travels to the next service in the traceparent header.
type SpanContext struct {
	TraceID [16]byte
	SpanID  [8]byte
	Sampled bool
}

// Valid reports whether both ids are set, as W3C Trace Context requires.
func (sc SpanContext) Valid() bool {
	return sc.TraceID != [16]byte{} && sc.SpanID != [8]byte{}
}

// Traceparent renders sc as a version 00 traceparent header.
func (sc SpanContext) Traceparent() string {
	flags := "00"
	if sc.Sampled {
		flags = "01"
	}
	return "00-" + hex.EncodeToString(sc.TraceID[:]) + "-" + hex.EncodeToString(sc.SpanID[:]) + "-" + flags
}

// ParseTraceparent reads a traceparent header. It reports false for a header that is missing
// or not valid, in which case W3C Trace Context says to start a new trace.
func ParseTraceparent(h string) (SpanContext, bool) {
	var sc SpanContext
	// version-traceid-spanid-flags: 2 + 1 + 32 + 1 + 16 + 1 + 2.
	if len(h) < 55 || h[2] != '-' || h[35] != '-' || h[52] != '-' {
		return sc, false
	}
	var version, flags [1]byte
	if !lowerHex(h[:55]) {
		return sc, false
	}
	if _, err := hex.Decode(version[:], []byte(h[:2])); err != nil || version[0] == 0xff {
		return sc, false
	}
	// Version 00 is exactly this long. A later version may add fields after another dash.
	if len(h) > 55 && (version[0] == 0 || h[55] != '-') {
		return sc, false
	}
	if _, err := hex.Decode(sc.TraceID[:], []byte(h[3:35])); err != nil {
		return sc, false
	}
	if _, err := hex.Decode(sc.SpanID[:], []byte(h[36:52])); err != nil {
		return sc, false
	}
	if _, err := hex.Decode(flags[:], []byte(h[53:55])); err != nil {
		return sc, false
	}
	sc.Sampled = flags[0]&1 == 1
	return sc, sc.Valid()
}

// lowerHex reports whether s holds only dashes and lowercase hex digits; the header may not
// use uppercase.
func lowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '-' && (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Attr is one attribute of a span. Exactly one of the values is used, as Kind says.
type Attr struct {
	Key   string
	Kind  AttrKind
	Str   string
	Int   int64
	Float float64
	Bool  bool
}

// AttrKind is the type of an attribute's value.
type AttrKind int

// The kinds of value an attribute can hold.
const (
	AttrString AttrKind = iota
	AttrInt
	AttrFloat
	AttrBool
)

// SpanData is a finished span on its way to an exporter.
type SpanData struct {
	TraceID    [16]byte
	SpanID     [8]byte
	ParentID   [8]byte // zero for the first span of a trace
	TraceState string
	Name       string
	Kind       SpanKind
	Start, End time.Time
	Attrs      []Attr
	Failed     bool
	Message    string
}

// Attr returns the attribute named key, for tests and exporters that look one up.
func (d SpanData) Attr(key string) (Attr, bool) {
	for _, a := range d.Attrs {
		if a.Key == key {
			return a, true
		}
	}
	return Attr{}, false
}

// SpanExporter takes finished spans in batches.
type SpanExporter interface {
	Export(ctx context.Context, spans []SpanData) error
}

// TracerConfig says where spans go.
type TracerConfig struct {
	// Endpoint is the base URL of an OTLP/HTTP collector, such as http://collector:4318.
	// Spans are posted to its /v1/traces. Empty, with no Exporter, means no tracing.
	Endpoint string
	// Headers are sent with every export, for a collector that needs a key.
	Headers map[string]string
	// SampleRatio is the share of traces that start here that are recorded, from 0 to 1.
	// Zero means all of them. A call that arrives with a traceparent follows its sampled flag.
	SampleRatio float64
	// Version is reported as service.version.
	Version string
	Client  *http.Client
	Logger  *slog.Logger
	// Dropped, when set, is told how many spans were lost to a full queue or a failed export.
	Dropped func(n int)
	// Exporter replaces the OTLP exporter built from Endpoint.
	Exporter SpanExporter
	// FlushEvery is how often queued spans are exported. Zero means every 5 seconds.
	FlushEvery time.Duration
	Now        func() time.Time
}

// Tracer makes spans and exports the finished ones in the background. A nil *Tracer makes
// nil spans, and every method of a nil *Span does nothing, so tracing that is off costs the
// code that reports to it a nil check.
type Tracer struct {
	exp     SpanExporter
	ratio   float64
	now     func() time.Time
	log     *slog.Logger
	dropped func(int)
	every   time.Duration

	queue chan SpanData
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
}

// NewTracer returns a tracer for cfg, or nil when cfg names no endpoint and no exporter.
func NewTracer(cfg TracerConfig) (*Tracer, error) {
	exp := cfg.Exporter
	if exp == nil {
		if cfg.Endpoint == "" {
			return nil, nil
		}
		target, err := tracesURL(cfg.Endpoint)
		if err != nil {
			return nil, err
		}
		client := cfg.Client
		if client == nil {
			client = &http.Client{}
		}
		exp = &otlpExporter{url: target, headers: cfg.Headers, client: client, version: cfg.Version}
	}
	if cfg.SampleRatio < 0 || cfg.SampleRatio > 1 {
		return nil, fmt.Errorf("otel.sample_ratio %v must be between 0 and 1", cfg.SampleRatio)
	}
	t := &Tracer{
		exp: exp, ratio: cfg.SampleRatio, now: cfg.Now, log: cfg.Logger, dropped: cfg.Dropped, every: cfg.FlushEvery,
		queue: make(chan SpanData, spanQueueSize), stop: make(chan struct{}), done: make(chan struct{}),
	}
	if t.ratio == 0 {
		t.ratio = 1
	}
	if t.now == nil {
		t.now = time.Now
	}
	if t.log == nil {
		t.log = slog.New(slog.DiscardHandler)
	}
	if t.every <= 0 {
		t.every = spanFlushEvery
	}
	go t.run()
	return t, nil
}

// tracesURL returns where an OTLP/HTTP collector at endpoint takes traces.
func tracesURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		// The value is not repeated: it can hold a collector's key.
		return "", errors.New("otel.endpoint must be an http or https URL of an OTLP/HTTP collector, usually port 4318; OTLP over gRPC is not supported")
	}
	if !strings.HasSuffix(u.Path, "/v1/traces") {
		u.Path = strings.TrimSuffix(u.Path, "/") + "/v1/traces"
	}
	return u.String(), nil
}

// CollectorHost returns the host of a collector URL, which is all of it that is safe to log:
// the user part and the query can hold a key.
func CollectorHost(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil {
		return ""
	}
	return u.Host
}

// run exports the queue in batches: when a batch is full, on every tick, and once more when
// the tracer shuts down.
func (t *Tracer) run() {
	defer close(t.done)
	tick := time.NewTicker(t.every)
	defer tick.Stop()
	batch := make([]SpanData, 0, spanBatchSize)
	failing := false
	flush := func() {
		if len(batch) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), spanExportTimeout)
		err := t.exp.Export(ctx, batch)
		cancel()
		switch {
		case err != nil:
			t.drop(len(batch))
			// One line when exports start to fail, not one per batch.
			if !failing {
				t.log.Warn("exporting spans failed, dropping them until it works again", "err", err)
			}
			failing = true
		case failing:
			t.log.Info("exporting spans works again")
			failing = false
		}
		batch = batch[:0]
	}
	for {
		select {
		case s := <-t.queue:
			if batch = append(batch, s); len(batch) >= spanBatchSize {
				flush()
			}
		case <-tick.C:
			flush()
		case <-t.stop:
			for {
				select {
				case s := <-t.queue:
					if batch = append(batch, s); len(batch) >= spanBatchSize {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

func (t *Tracer) drop(n int) {
	if t.dropped != nil {
		t.dropped(n)
	}
}

// Shutdown exports what is queued and stops the tracer. Spans ended after it are dropped.
func (t *Tracer) Shutdown(ctx context.Context) error {
	if t == nil {
		return nil
	}
	t.once.Do(func() { close(t.stop) })
	select {
	case <-t.done:
		return nil
	case <-ctx.Done():
		return errors.New("telemetry: the last spans were not exported in time")
	}
}

// Start begins a span. traceparent and tracestate are the headers of the call that caused
// it: with a valid traceparent the span joins that trace and follows its sampled flag,
// otherwise it starts a trace of its own.
func (t *Tracer) Start(name string, kind SpanKind, traceparent, tracestate string) *Span {
	if t == nil {
		return nil
	}
	s := &Span{t: t}
	if parent, ok := ParseTraceparent(traceparent); ok {
		s.data.TraceID, s.data.ParentID, s.data.TraceState = parent.TraceID, parent.SpanID, boundedState(tracestate)
		s.recording = parent.Sampled
	} else {
		binary.BigEndian.PutUint64(s.data.TraceID[:8], rand.Uint64()) //nolint:gosec // an id has to be unique, not secret
		binary.BigEndian.PutUint64(s.data.TraceID[8:], nonZero())
		s.recording = t.ratio >= 1 || rand.Float64() < t.ratio //nolint:gosec // sampling is no secret
	}
	s.begin(name, kind)
	return s
}

// maxTraceState is the longest tracestate W3C Trace Context asks a service to carry.
const maxTraceState = 512

// boundedState returns a caller's tracestate for the spans, or "" when it is longer than
// the standard allows or holds anything but printable ASCII. The header comes from outside
// and is copied onto every span of the call, so its size must not be the caller's choice.
func boundedState(s string) string {
	if len(s) > maxTraceState {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < ' ' || s[i] > '~' {
			return ""
		}
	}
	return s
}

func nonZero() uint64 {
	for {
		if n := rand.Uint64(); n != 0 { //nolint:gosec // an id has to be unique, not secret
			return n
		}
	}
}

// Span is one timed operation. It is used by one goroutine at a time.
type Span struct {
	t         *Tracer
	data      SpanData
	recording bool
	ended     bool
}

func (s *Span) begin(name string, kind SpanKind) {
	binary.BigEndian.PutUint64(s.data.SpanID[:], nonZero())
	if s.recording {
		s.data.Name, s.data.Kind, s.data.Start = name, kind, s.t.now()
	}
}

// Child begins a span under s, in the same trace.
func (s *Span) Child(name string, kind SpanKind) *Span {
	if s == nil {
		return nil
	}
	c := &Span{t: s.t, recording: s.recording}
	c.data.TraceID, c.data.ParentID, c.data.TraceState = s.data.TraceID, s.data.SpanID, s.data.TraceState
	c.begin(name, kind)
	return c
}

// Context returns what the next service needs to continue the trace under s.
func (s *Span) Context() SpanContext {
	if s == nil {
		return SpanContext{}
	}
	return SpanContext{TraceID: s.data.TraceID, SpanID: s.data.SpanID, Sampled: s.recording}
}

// Traceparent returns the header to send with a call made under s, or "" for a nil span.
func (s *Span) Traceparent() string {
	if s == nil {
		return ""
	}
	return s.Context().Traceparent()
}

// SetName renames the span, for a name that is only known once the work is under way.
func (s *Span) SetName(name string) {
	if s != nil && s.recording {
		s.data.Name = name
	}
}

// SetString adds a string attribute. An empty value is left out.
func (s *Span) SetString(key, v string) {
	if s != nil && s.recording && v != "" {
		s.data.Attrs = append(s.data.Attrs, Attr{Key: key, Kind: AttrString, Str: v})
	}
}

// SetInt adds an integer attribute.
func (s *Span) SetInt(key string, v int64) {
	if s != nil && s.recording {
		s.data.Attrs = append(s.data.Attrs, Attr{Key: key, Kind: AttrInt, Int: v})
	}
}

// SetFloat adds a floating point attribute.
func (s *Span) SetFloat(key string, v float64) {
	if s != nil && s.recording {
		s.data.Attrs = append(s.data.Attrs, Attr{Key: key, Kind: AttrFloat, Float: v})
	}
}

// SetBool adds a boolean attribute.
func (s *Span) SetBool(key string, v bool) {
	if s != nil && s.recording {
		s.data.Attrs = append(s.data.Attrs, Attr{Key: key, Kind: AttrBool, Bool: v})
	}
}

// Fail marks the span as an error. message is a short fixed reason, never text from a
// request or a provider's answer.
func (s *Span) Fail(message string) {
	if s != nil && s.recording {
		s.data.Failed, s.data.Message = true, message
	}
}

// End finishes the span and queues it for export. A span that is not sampled, or that has
// ended already, is not exported. A full queue drops the span rather than block the request.
func (s *Span) End() {
	if s == nil || !s.recording || s.ended {
		return
	}
	s.ended = true
	s.data.End = s.t.now()
	select {
	case s.t.queue <- s.data:
	default:
		s.t.drop(1)
	}
}
