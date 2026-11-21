package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/provider"
	"github.com/raven-clown/transponder/internal/telemetry"
)

func TestAuthFailedWarnsOnceAboutTrailingSlashIssuer(t *testing.T) {
	var logs bytes.Buffer
	g := &gateway{d: Deps{Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}}
	err := fmt.Errorf("%w: issuer is %q, want %q: %w; set auth.oidc.issuer to %q",
		auth.ErrUnauthenticated, "https://x/", "https://x", auth.ErrIssuerTrailingSlash, "https://x/")
	write := func(http.ResponseWriter, int, string, string, string, string) {}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil)
	for range 2 {
		g.authFailed(httptest.NewRecorder(), req, err, write)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("warn count = %d, want 1:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "level=DEBUG"); n != 2 {
		t.Fatalf("debug count = %d, want one per refusal:\n%s", n, logs.String())
	}
}

func TestFallbackReason(t *testing.T) {
	upstream := func(status int) error {
		return &provider.Error{Provider: "p", Status: status, Err: errors.New("upstream answered")}
	}
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"attempt clock", errAttemptTimeout, "timeout"},
		{"attempt clock before the first event", fmt.Errorf("stream ended before its first event: %w", errAttemptTimeout), "timeout"},
		{"attempt cancelled while connecting", &provider.Error{Provider: "p", Retryable: true, Err: context.Canceled}, "timeout"},
		{"deadline", context.DeadlineExceeded, "timeout"},
		{"connection refused", &provider.Error{Provider: "p", Retryable: true, Err: errors.New("connection refused")}, "unreachable"},
		{"429", upstream(http.StatusTooManyRequests), "rate_limited"},
		{"401", upstream(http.StatusUnauthorized), "auth"},
		{"403", upstream(http.StatusForbidden), "auth"},
		{"408", upstream(http.StatusRequestTimeout), "timeout"},
		{"500", upstream(http.StatusInternalServerError), "server_error"},
		{"529", upstream(529), "server_error"},
		{"wrapped 503", fmt.Errorf("stream ended before its first event: %w", upstream(http.StatusServiceUnavailable)), "server_error"},
		{"stream closed early", fmt.Errorf("stream ended before its first event: %w", io.EOF), "error"},
		{"another status", upstream(http.StatusConflict), "error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallbackReason(tt.err); got != tt.want {
				t.Fatalf("fallbackReason = %q, want %q", got, tt.want)
			}
		})
	}
}

func scrapeMetrics(m *telemetry.Metrics) string {
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func TestObserveStatusCodes(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"explicit status", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }, "418"},
		{"body without a status", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hi") }, "200"},
		{"nothing written", func(http.ResponseWriter, *http.Request) {}, "200"},
		{"first status wins", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusAccepted)
			w.WriteHeader(http.StatusInternalServerError)
		}, "202"},
		{"interim status is not the answer", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusNoContent)
		}, "204"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &gateway{d: Deps{Metrics: telemetry.NewMetrics("test"), Now: time.Now}}
			g.observe(tt.handler).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil))
			want := `transponder_requests_total{endpoint="other",route="",provider="",model="",code="` + tt.want + `"} 1` + "\n"
			if got := scrapeMetrics(g.d.Metrics); !strings.Contains(got, want) {
				t.Fatalf("missing %q in:\n%s", want, got)
			}
		})
	}
}

// A handler that panics is counted as a 500 and no longer in flight, and the panic goes on
// to the server as before.
func TestObserveCountsPanicAs500(t *testing.T) {
	g := &gateway{d: Deps{Metrics: telemetry.NewMetrics("test"), Now: time.Now}}
	h := g.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Fatalf("recovered %v, want the handler's panic", r)
			}
		}()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil))
	}()
	got := scrapeMetrics(g.d.Metrics)
	for _, want := range []string{
		`transponder_requests_total{endpoint="other",route="",provider="",model="",code="500"} 1`,
		`transponder_inflight_requests 0`,
	} {
		if !strings.Contains(got, want+"\n") {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
}

func TestObserveWithoutMetricsAddsNothing(t *testing.T) {
	g := &gateway{}
	var seen http.ResponseWriter
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = w
		if observedFrom(r.Context()) != nil {
			t.Error("the request is observed with no metrics")
		}
	})
	rec := httptest.NewRecorder()
	g.observe(next).ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/x", nil))
	if seen != http.ResponseWriter(rec) {
		t.Fatal("the writer was wrapped with no metrics")
	}
}

func TestRawWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	if got := rawWriter(&statusWriter{ResponseWriter: rec}); got != http.ResponseWriter(rec) {
		t.Fatal("rawWriter did not unwrap the status writer")
	}
	if got := rawWriter(rec); got != http.ResponseWriter(rec) {
		t.Fatal("rawWriter changed a plain writer")
	}
}
