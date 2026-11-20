package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/telemetry"
)

func TestAdminMounted(t *testing.T) {
	var gotPath, gotID string
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotID = r.URL.Path, r.Header.Get("X-Request-ID")
		w.WriteHeader(http.StatusTeapot)
	})
	h := New(Deps{Admin: admin})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/v1/keys", nil))
	if rec.Code != http.StatusTeapot || gotPath != "/admin/v1/keys" || gotID == "" || gotID != rec.Header().Get("X-Request-ID") {
		t.Fatalf("status %d, path %q, request id %q", rec.Code, gotPath, gotID)
	}

	m := telemetry.NewMetrics("test")
	New(Deps{Admin: admin, Metrics: m, Now: time.Now}).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/v1/keys", nil))
	if want := `transponder_requests_total{endpoint="admin",route="",provider="",model="",code="418"} 1`; !strings.Contains(scrapeMetrics(m), want) {
		t.Fatalf("missing %q in:\n%s", want, scrapeMetrics(m))
	}

	rec = httptest.NewRecorder()
	New(Deps{}).ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/admin/v1/keys", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("without an admin API: status %d", rec.Code)
	}
}
