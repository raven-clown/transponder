package admin

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/auth"
)

func TestDenyWarnsOnceAboutTrailingSlashIssuer(t *testing.T) {
	var logs bytes.Buffer
	h := &handler{d: Deps{Logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}}
	err := fmt.Errorf("%w: issuer is %q, want %q: %w; set auth.oidc.issuer to %q",
		auth.ErrUnauthenticated, "https://x/", "https://x", auth.ErrIssuerTrailingSlash, "https://x/")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/v1/status", nil)
	for range 2 {
		rec := httptest.NewRecorder()
		h.deny(rec, req, err)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Fatalf("warn count = %d, want 1:\n%s", n, logs.String())
	}
	if n := strings.Count(logs.String(), "level=DEBUG"); n != 2 {
		t.Fatalf("debug count = %d, want one per refusal:\n%s", n, logs.String())
	}
}
