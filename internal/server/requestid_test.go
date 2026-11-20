package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRequestIDAllocations keeps the middleware on every call cheap. A header key that is not in
// Go's canonical form ("X-Request-ID") is copied on every Get and Set.
func TestRequestIDAllocations(t *testing.T) {
	h := RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_ = r.Header.Get(requestIDHeader)
	}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	req.Header.Set(requestIDHeader, "caller-id")
	w := httptest.NewRecorder()
	// The response header's value, and the context value, its context and the request copy
	// that carries it.
	const want = 4
	if n := testing.AllocsPerRun(100, func() { h.ServeHTTP(w, req) }); n > want {
		t.Fatalf("RequestID allocates %v times per call, want at most %d", n, want)
	}
}

func TestRequestID(t *testing.T) {
	tests := []struct {
		name string
		id   string
		set  bool
		keep bool
	}{
		{name: "kept when valid", id: "abc-123", set: true, keep: true},
		{name: "kept at one character", id: "x", set: true, keep: true},
		{name: "kept at 128 characters", id: strings.Repeat("a", 128), set: true, keep: true},
		{name: "replaced when empty", id: "", set: true, keep: false},
		{name: "replaced when missing", keep: false},
		{name: "replaced when too long", id: strings.Repeat("a", 129), set: true, keep: false},
		{name: "replaced when it contains a control character", id: "bad\nid", set: true, keep: false},
		{name: "replaced when it contains DEL", id: "bad\x7fid", set: true, keep: false},
		{name: "replaced when it is not ASCII", id: "caf\u00e9", set: true, keep: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got, during, onRequest string
			h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				during = w.Header().Get("X-Request-ID")
				got = RequestIDFrom(r.Context())
				onRequest = r.Header.Get("X-Request-ID")
			}))
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			if tt.set {
				req.Header["X-Request-Id"] = []string{tt.id}
			}
			h.ServeHTTP(rec, req)
			if got == "" || got != during || rec.Header().Get("X-Request-ID") != got || onRequest != got {
				t.Fatalf("context %q during %q response %q request %q", got, during, rec.Header().Get("X-Request-ID"), onRequest)
			}
			if tt.keep {
				if got != tt.id {
					t.Fatalf("id = %q, want %q", got, tt.id)
				}
				return
			}
			if got == tt.id || !lowerHex32(got) {
				t.Fatalf("id = %q, want a new 32-character hex id", got)
			}
		})
	}
}

func TestRequestIDGeneratedValuesDiffer(t *testing.T) {
	one := generatedRequestID(t)
	two := generatedRequestID(t)
	if one == two || !lowerHex32(one) || !lowerHex32(two) {
		t.Fatalf("ids %q and %q", one, two)
	}
}

func TestRequestIDFromBareContext(t *testing.T) {
	if got := RequestIDFrom(context.Background()); got != "" {
		t.Fatalf("RequestIDFrom = %q, want empty", got)
	}
}

func generatedRequestID(t *testing.T) string {
	t.Helper()
	var got string
	RequestID(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = RequestIDFrom(r.Context())
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	return got
}

func lowerHex32(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func TestNewSetsRequestID(t *testing.T) {
	for _, d := range []Deps{{}, {RequestTimeout: time.Minute}} {
		rec := httptest.NewRecorder()
		New(d).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
		if !lowerHex32(rec.Header().Get("X-Request-ID")) {
			t.Fatalf("X-Request-ID = %q", rec.Header().Get("X-Request-ID"))
		}
	}
}
