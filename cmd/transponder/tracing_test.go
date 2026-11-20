package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/raven-clown/transponder/internal/config"
	"github.com/raven-clown/transponder/internal/telemetry"
)

func TestStartTracing(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	tracer, flush, err := startTracing(config.OTel{}, telemetry.NewMetrics("test"), logger)
	if tracer != nil || err != nil {
		t.Fatalf("with no endpoint: tracer %v, err %v; want neither", tracer, err)
	}
	flush()
	if _, _, err := startTracing(config.OTel{Endpoint: "collector:4317"}, nil, logger); err == nil || !strings.Contains(err.Error(), "otel.endpoint") {
		t.Fatalf("err = %v, want it to name otel.endpoint", err)
	}
}

// collector is an OTLP/HTTP collector that keeps the names of the spans it is sent.
type collector struct {
	srv   *httptest.Server
	mu    sync.Mutex
	names []string
	auth  []string
}

func newCollector(t *testing.T, status int) *collector {
	t.Helper()
	c := &collector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						Name string `json:"name"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if r.URL.Path != "/v1/traces" || json.NewDecoder(r.Body).Decode(&body) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		c.auth = append(c.auth, r.Header.Get("Authorization"))
		for _, rs := range body.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					c.names = append(c.names, s.Name)
				}
			}
		}
		c.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// With otel.endpoint set, a call's spans reach the collector by the time the gateway has
// stopped.
func TestServeExportsSpans(t *testing.T) {
	col := newCollector(t, http.StatusOK)
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Bootstrap{
		ListenAddr: ln.Addr().String(), Store: config.Store{SQLite: filepath.Join(t.TempDir(), "t.db")}, LogLevel: "info",
		OTel: config.OTel{Endpoint: col.srv.URL, Headers: map[string]string{"Authorization": "Bearer otel-key"}},
	}
	key, stop := startServe(t, cfg, ln)
	if code := chat(t, ln.Addr().String(), key); code != http.StatusOK {
		t.Fatalf("chat status %d", code)
	}
	stop()

	col.mu.Lock()
	defer col.mu.Unlock()
	got := strings.Join(col.names, ", ")
	for _, want := range []string{"POST /v1/chat/completions", "chat m"} {
		if !strings.Contains(got, want) {
			t.Errorf("the collector has no span %q; it has: %s", want, got)
		}
	}
	for _, a := range col.auth {
		if a != "Bearer otel-key" {
			t.Errorf("an export carried Authorization %q", a)
		}
	}
}

// Spans a collector refuses are lost, and the loss shows in the metrics.
func TestStartTracingCountsSpansTheCollectorRefuses(t *testing.T) {
	col := newCollector(t, http.StatusServiceUnavailable)
	m := telemetry.NewMetrics("test")
	tracer, flush, err := startTracing(config.OTel{Endpoint: col.srv.URL}, m, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil || tracer == nil {
		t.Fatalf("tracer %v, err %v", tracer, err)
	}
	tracer.Start("a", telemetry.SpanServer, "", "").End()
	tracer.Start("b", telemetry.SpanServer, "", "").End()
	flush()

	col.mu.Lock()
	sent := len(col.names)
	col.mu.Unlock()
	if sent != 2 {
		t.Fatalf("the collector was sent %d spans, want 2", sent)
	}
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `transponder_events_dropped_total{sink="otlp"} 2`+"\n") {
		t.Fatalf("the refused spans are not counted under the otlp sink:\n%s", rec.Body)
	}
}
