package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/telemetry"
)

func TestNoSniff(t *testing.T) {
	e := setup(t, echo.New("echo"))
	withKey := map[string]string{"Authorization": "Bearer " + e.key}
	chat := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	stream := `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	tests := []struct {
		name    string
		method  string
		path    string
		headers map[string]string
		body    string
		status  int
	}{
		{name: "chat", method: http.MethodPost, path: "/v1/chat/completions", headers: withKey, body: chat, status: http.StatusOK},
		{name: "chat stream", method: http.MethodPost, path: "/v1/chat/completions", headers: withKey, body: stream, status: http.StatusOK},
		{name: "messages", method: http.MethodPost, path: "/v1/messages", headers: withKey, body: chat, status: http.StatusOK},
		{name: "models", method: http.MethodGet, path: "/v1/models", headers: withKey, status: http.StatusOK},
		{name: "no key", method: http.MethodPost, path: "/v1/chat/completions", body: chat, status: http.StatusUnauthorized},
		{name: "bad body", method: http.MethodPost, path: "/v1/chat/completions", headers: withKey, body: "{", status: http.StatusBadRequest},
		{name: "unknown path", method: http.MethodGet, path: "/nope", status: http.StatusNotFound},
		{name: "healthz", method: http.MethodGet, path: "/healthz", status: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := e.doPath(tt.method, tt.path, tt.headers, tt.body)
			if err != nil {
				t.Fatal(err)
			}
			if res.status != tt.status {
				t.Fatalf("status %d, want %d: %s", res.status, tt.status, res.body)
			}
			if got := res.header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options %q, want nosniff", got)
			}
		})
	}
}

func TestNoSniffOps(t *testing.T) {
	h := server.Ops(nil, telemetry.NewMetrics("test"))
	for _, path := range []string{"/healthz", "/readyz", "/metrics", "/nope"} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil))
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options %q, want nosniff", got)
			}
		})
	}
}
