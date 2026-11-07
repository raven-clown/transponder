package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type pingFunc func(ctx context.Context) error

func (f pingFunc) Ping(ctx context.Context) error { return f(ctx) }

var (
	okPing   = pingFunc(func(context.Context) error { return nil })
	failPing = pingFunc(func(context.Context) error { return errors.New("down") })
	// hangPing waits for the deadline, like a database that stopped answering.
	hangPing = pingFunc(func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
)

func probe(t *testing.T, h http.Handler, method string) (int, string, http.Header) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, "/", nil))
	return rec.Code, strings.TrimSpace(rec.Body.String()), rec.Header()
}

func TestHealthz(t *testing.T) {
	code, body, hdr := probe(t, Healthz(), http.MethodGet)
	if code != http.StatusOK || body != `{"status":"ok"}` || hdr.Get("Content-Type") != "application/json" {
		t.Fatalf("%d %s %v", code, body, hdr)
	}
}

func TestReadyz(t *testing.T) {
	orig := readyTimeout
	readyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { readyTimeout = orig })
	tests := []struct {
		name   string
		checks map[string]Pinger
		code   int
		body   string
	}{
		{"no checks", nil, http.StatusOK, `{"status":"ok"}`},
		{"all pass", map[string]Pinger{"store": okPing, "cache": okPing}, http.StatusOK, `{"status":"ok"}`},
		{"one fails", map[string]Pinger{"store": failPing, "cache": okPing}, http.StatusServiceUnavailable, `{"failed":["store"],"status":"unavailable"}`},
		{"names are sorted", map[string]Pinger{"store": failPing, "bus": hangPing, "cache": okPing}, http.StatusServiceUnavailable, `{"failed":["bus","store"],"status":"unavailable"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body, _ := probe(t, Readyz(tt.checks), http.MethodGet)
			if code != tt.code || body != tt.body {
				t.Fatalf("got %d %s, want %d %s", code, body, tt.code, tt.body)
			}
		})
	}
}

func TestHealthWrongMethod(t *testing.T) {
	for _, h := range []http.Handler{Healthz(), Readyz(nil)} {
		code, _, hdr := probe(t, h, http.MethodPost)
		if code != http.StatusMethodNotAllowed || hdr.Get("Allow") != "GET, HEAD" {
			t.Fatalf("%d %v", code, hdr)
		}
		if code, _, _ := probe(t, h, http.MethodHead); code != http.StatusOK {
			t.Fatalf("HEAD answered %d", code)
		}
	}
}

func TestHealthNeedsNoKey(t *testing.T) {
	h := New(Deps{Ready: map[string]Pinger{"store": failPing}})
	for path, want := range map[string]int{"/healthz": http.StatusOK, "/readyz": http.StatusServiceUnavailable} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
		if rec.Code != want {
			t.Fatalf("%s answered %d, want %d", path, rec.Code, want)
		}
	}
}
