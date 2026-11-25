package server_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/pricing"
	"github.com/raven-clown/transponder/internal/provider/echo"
	"github.com/raven-clown/transponder/internal/router"
	"github.com/raven-clown/transponder/internal/server"
	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
	"github.com/raven-clown/transponder/internal/telemetry"
)

// metered is a gateway whose handler is called directly, so a request has been counted by
// the time its call returns.
type metered struct {
	h    http.Handler
	m    *telemetry.Metrics
	db   *sqlite.DB
	keys map[auth.Role]string
}

func setupMetered(t *testing.T, routes []router.Route, change func(*server.Deps)) *metered {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	e := &metered{m: telemetry.NewMetrics("test"), db: db, keys: map[auth.Role]string{}}
	for _, role := range []auth.Role{auth.RoleMember, auth.RoleViewer, auth.RoleAdmin} {
		plain, hash, last4, err := auth.NewKey()
		if err != nil {
			t.Fatal(err)
		}
		k := store.APIKey{OrgID: store.DefaultOrg, Name: string(role), Hash: hash, Last4: last4, Role: string(role), CreatedAt: time.Now()}
		if err := db.CreateKey(ctx, k); err != nil {
			t.Fatal(err)
		}
		e.keys[role] = plain
	}
	deps := server.Deps{
		Auth:    auth.NewAuthenticator(db, time.Now),
		Router:  router.New(routes),
		Prices:  pricing.Default(),
		Usage:   db,
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Now:     time.Now,
		Metrics: e.m,
	}
	if change != nil {
		change(&deps)
	}
	e.h = server.New(deps)
	return e
}

// call sends one request as the member key, or with no key when key is empty.
func (e *metered) call(method, path, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	return rec
}

func (e *metered) chat(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	return e.call(http.MethodPost, "/v1/chat/completions", e.keys[auth.RoleMember], body)
}

func (e *metered) scrape(t *testing.T) string {
	t.Helper()
	rec := httptest.NewRecorder()
	e.m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

func wantMetrics(t *testing.T, got string, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if !strings.Contains(got, line+"\n") {
			t.Errorf("missing %q", line)
		}
	}
	if t.Failed() {
		t.Fatalf("metrics:\n%s", got)
	}
}

func TestMetricsAfterFallback(t *testing.T) {
	a := countedUpstream(t, status(500, "oops"))
	b := countedUpstream(t, status(200, okChat))
	e := setupMetered(t, route("fast", oaTarget("a", "gpt-5", a), oaTarget("b", "gpt-4o-mini", b)), nil)
	if rec := e.chat(t, `{"model":"fast","messages":[{"role":"user","content":"ping"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := e.scrape(t)
	wantMetrics(t, got,
		`transponder_requests_total{endpoint="chat.completions",route="fast",provider="b",model="gpt-4o-mini",code="200"} 1`,
		`transponder_request_duration_seconds_count{endpoint="chat.completions",route="fast"} 1`,
		`transponder_inflight_requests 0`,
		`transponder_tokens_total{direction="input",provider="b",model="gpt-4o-mini"} 1000`,
		`transponder_tokens_total{direction="output",provider="b",model="gpt-4o-mini"} 500`,
		`transponder_cost_usd_total{provider="b",model="gpt-4o-mini"} 0.00045`,
		`transponder_upstream_duration_seconds_count{provider="a",model="gpt-5"} 1`,
		`transponder_upstream_duration_seconds_count{provider="b",model="gpt-4o-mini"} 1`,
		`transponder_fallbacks_total{route="fast",from="a",to="b",reason="server_error"} 1`,
		`transponder_unpriced_requests_total 0`,
	)
	// The failed target carried no tokens and answered no request.
	for _, absent := range []string{`transponder_tokens_total{direction="input",provider="a"`, `transponder_requests_total{endpoint="chat.completions",route="fast",provider="a"`} {
		if strings.Contains(got, absent) {
			t.Errorf("unexpected series %s", absent)
		}
	}
}

func TestMetricsWhenEveryTargetFails(t *testing.T) {
	a := countedUpstream(t, status(503, "down"))
	b := countedUpstream(t, status(429, "slow down"))
	e := setupMetered(t, route("fast", oaTarget("a", "gpt-5", a), oaTarget("b", "gpt-4o-mini", b)), nil)
	if rec := e.chat(t, `{"model":"fast","messages":[{"role":"user","content":"ping"}]}`); rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	wantMetrics(t, e.scrape(t),
		`transponder_requests_total{endpoint="chat.completions",route="fast",provider="b",model="gpt-4o-mini",code="502"} 1`,
		`transponder_fallbacks_total{route="fast",from="a",to="b",reason="server_error"} 1`,
		`transponder_upstream_duration_seconds_count{provider="a",model="gpt-5"} 1`,
		`transponder_upstream_duration_seconds_count{provider="b",model="gpt-4o-mini"} 1`,
	)
	// The last target had nowhere to fall back to.
	if got := e.scrape(t); strings.Contains(got, `from="b"`) {
		t.Fatalf("a fallback was counted from the last target:\n%s", got)
	}
}

// Requests that end before a provider is asked are counted without one, and a model name
// that matched no route never becomes a label.
func TestMetricsForRefusedRequests(t *testing.T) {
	up := countedUpstream(t, status(200, okChat))
	e := setupMetered(t, route("fast", oaTarget("a", "gpt-4o-mini", up)), nil)
	member := e.keys[auth.RoleMember]

	if rec := e.call(http.MethodPost, "/v1/chat/completions", "", ping); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no key: status %d", rec.Code)
	}
	if rec := e.chat(t, `{"model":"no-such-model-1234","messages":[]}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model: status %d", rec.Code)
	}
	if rec := e.chat(t, `{not json`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: status %d", rec.Code)
	}
	if rec := e.call(http.MethodGet, "/v1/models", member, ""); rec.Code != http.StatusOK {
		t.Fatalf("models: status %d", rec.Code)
	}
	if rec := e.call(http.MethodGet, "/nowhere", member, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path: status %d", rec.Code)
	}
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		e.call(http.MethodGet, path, e.keys[auth.RoleViewer], "")
	}

	got := e.scrape(t)
	wantMetrics(t, got,
		`transponder_requests_total{endpoint="chat.completions",route="",provider="",model="",code="401"} 1`,
		`transponder_requests_total{endpoint="chat.completions",route="",provider="",model="",code="404"} 1`,
		`transponder_requests_total{endpoint="chat.completions",route="",provider="",model="",code="400"} 1`,
		`transponder_requests_total{endpoint="models",route="",provider="",model="",code="200"} 1`,
		`transponder_requests_total{endpoint="other",route="",provider="",model="",code="404"} 1`,
		`transponder_inflight_requests 0`,
	)
	if strings.Contains(got, "no-such-model-1234") {
		t.Error("a model name from a caller that matched no route is in the metrics")
	}
	// Probes and scrapes are not gateway requests: nothing else was counted.
	if n := strings.Count(got, "transponder_requests_total{"); n != 5 {
		t.Errorf("%d request series, want 5:\n%s", n, got)
	}
	if up.calls.Load() != 0 {
		t.Errorf("the provider was called %d times", up.calls.Load())
	}
}

func TestMetricsForStreamMatchUsageRecord(t *testing.T) {
	e := setupMetered(t, echoRoutes(echo.New("echo")), nil)
	rec := e.chat(t, `{"model":"m","stream":true,"messages":[{"role":"user","content":"count these words please"}]}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "[DONE]") {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	recs, err := e.db.RecentUsage(context.Background(), store.DefaultOrg, 10)
	if err != nil || len(recs) != 1 || recs[0].InputTokens == 0 || recs[0].OutputTokens == 0 {
		t.Fatalf("usage records %+v, err %v", recs, err)
	}
	// "m" is the caller's own name for the model, and it is a label because echo answered.
	wantMetrics(t, e.scrape(t),
		`transponder_requests_total{endpoint="chat.completions",route="*",provider="echo",model="m",code="200"} 1`,
		fmt.Sprintf(`transponder_tokens_total{direction="input",provider="echo",model="m"} %d`, recs[0].InputTokens),
		fmt.Sprintf(`transponder_tokens_total{direction="output",provider="echo",model="m"} %d`, recs[0].OutputTokens),
		`transponder_upstream_duration_seconds_count{provider="echo",model="m"} 1`,
	)
}

func TestMetricsCountLimitRefusals(t *testing.T) {
	up := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`)
	})
	cfg := limits.Config{Budgets: []limits.Budget{{Subject: "user:*", Window: limits.Month, LimitNUSD: 100_000_000}}}
	e := setupMetered(t, route("fast", oaTarget("a", "gpt-4o-mini", up)), func(d *server.Deps) {
		d.Limiter = limits.New(cfg, limits.NewMemoryStore(), time.Now)
	})
	body := `{"model":"fast","messages":[{"role":"user","content":"ping"}]}`
	if rec := e.chat(t, body); rec.Code != http.StatusOK {
		t.Fatalf("first call %d: %s", rec.Code, rec.Body)
	}
	for range 2 {
		if rec := e.chat(t, body); rec.Code != http.StatusPaymentRequired {
			t.Fatalf("status %d: %s", rec.Code, rec.Body)
		}
	}
	wantMetrics(t, e.scrape(t),
		`transponder_limited_total{reason="budget_exceeded"} 2`,
		// A refusal is not counted against the provider that was never asked.
		`transponder_requests_total{endpoint="chat.completions",route="fast",provider="",model="",code="402"} 2`,
		`transponder_requests_total{endpoint="chat.completions",route="fast",provider="a",model="gpt-4o-mini",code="200"} 1`,
		`transponder_cost_usd_total{provider="a",model="gpt-4o-mini"} 0.15`,
	)
}

func TestMetricsCountUnpricedCalls(t *testing.T) {
	up := countedUpstream(t, status(200, okChat))
	e := setupMetered(t, route("fast", oaTarget("a", "a-model-with-no-price", up)), nil)
	if rec := e.chat(t, `{"model":"fast","messages":[{"role":"user","content":"ping"}]}`); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	got := e.scrape(t)
	wantMetrics(t, got, `transponder_unpriced_requests_total 1`)
	if strings.Contains(got, "transponder_cost_usd_total{") {
		t.Fatalf("an unpriced call added cost:\n%s", got)
	}
}

// brokenUsage fails every write, as a full or locked store would.
type brokenUsage struct{ store.UsageStore }

func (brokenUsage) RecordUsage(context.Context, store.UsageRecord) error {
	return errors.New("the disk is full")
}

func TestMetricsCountDroppedUsageRecords(t *testing.T) {
	e := setupMetered(t, echoRoutes(echo.New("echo")), func(d *server.Deps) {
		d.Usage = brokenUsage{d.Usage}
	})
	if rec := e.chat(t, ping); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	wantMetrics(t, e.scrape(t), `transponder_events_dropped_total{sink="sql"} 1`)
}

// The gauge is read while a call is held open at the provider, then after it has ended.
func TestMetricsInflight(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	up := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, okChat)
	})
	e := setupMetered(t, route("fast", oaTarget("a", "gpt-4o-mini", up)), nil)
	done := make(chan int, 1)
	go func() {
		done <- e.chat(t, `{"model":"fast","messages":[{"role":"user","content":"ping"}]}`).Code
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the provider was not called")
	}
	wantMetrics(t, e.scrape(t), `transponder_inflight_requests 1`)
	close(release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	wantMetrics(t, e.scrape(t), `transponder_inflight_requests 0`)
}

func TestMetricsEndpointNeedsViewer(t *testing.T) {
	e := setupMetered(t, echoRoutes(echo.New("echo")), nil)
	tests := []struct {
		name   string
		key    string
		status int
		code   string
	}{
		{"no key", "", http.StatusUnauthorized, "invalid_api_key"},
		{"wrong key", "tp_nope", http.StatusUnauthorized, "invalid_api_key"},
		{"member", e.keys[auth.RoleMember], http.StatusForbidden, "forbidden"},
		{"viewer", e.keys[auth.RoleViewer], http.StatusOK, ""},
		{"admin", e.keys[auth.RoleAdmin], http.StatusOK, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := e.call(http.MethodGet, "/metrics", tt.key, "")
			if rec.Code != tt.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tt.status, rec.Body)
			}
			if tt.status == http.StatusOK {
				if !strings.Contains(rec.Body.String(), "transponder_build_info{version=\"test\"} 1\n") {
					t.Fatalf("body:\n%s", rec.Body)
				}
				return
			}
			if got := errorCode(t, result{body: rec.Body.Bytes()}); got != tt.code {
				t.Fatalf("error code %q, want %q", got, tt.code)
			}
			if strings.Contains(rec.Body.String(), "transponder_") {
				t.Fatalf("metrics leaked to a refused caller:\n%s", rec.Body)
			}
		})
	}
	t.Run("x-api-key", func(t *testing.T) {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil)
		req.Header.Set("X-Api-Key", e.keys[auth.RoleViewer])
		rec := httptest.NewRecorder()
		e.h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	})
	t.Run("post", func(t *testing.T) {
		if rec := e.call(http.MethodPost, "/metrics", e.keys[auth.RoleViewer], ""); rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status %d, want 405", rec.Code)
		}
	})
}

func TestNoMetricsEndpointWithoutMetrics(t *testing.T) {
	e := setupMetered(t, echoRoutes(echo.New("echo")), func(d *server.Deps) { d.Metrics = nil })
	if rec := e.call(http.MethodGet, "/metrics", e.keys[auth.RoleAdmin], ""); rec.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", rec.Code)
	}
	if rec := e.chat(t, ping); rec.Code != http.StatusOK {
		t.Fatalf("chat status %d", rec.Code)
	}
}

func TestOpsServesMetricsWithoutKey(t *testing.T) {
	m := telemetry.NewMetrics("ops")
	m.Limited("rate_limited")
	h := server.Ops(map[string]server.Pinger{}, m)
	for path, want := range map[string]string{
		"/metrics": `transponder_limited_total{reason="rate_limited"} 1`,
		"/healthz": `"status":"ok"`,
		"/readyz":  `"status":"ok"`,
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), want) {
			t.Errorf("%s: status %d, body %s", path, rec.Code, rec.Body)
		}
	}
	rec := httptest.NewRecorder()
	server.Ops(nil, nil).ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("without metrics: status %d, want 404", rec.Code)
	}
	// Nothing but the probes and the metrics is served there.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/chat/completions", strings.NewReader(ping)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("chat on the ops handler: status %d, want 404", rec.Code)
	}
}

// A refused credential is counted by its reason, and every reason is in the scrape from the
// start, at 0. The key that was refused is never in a label.
func TestMetricsForRefusedCredentials(t *testing.T) {
	up := countedUpstream(t, status(200, okChat))
	e := setupMetered(t, route("fast", oaTarget("a", "gpt-4o-mini", up)), nil)

	if got := e.scrape(t); !strings.Contains(got, `transponder_auth_refused_total{reason="unknown_key"} 0`) {
		t.Fatalf("reasons are not in the scrape before any refusal:\n%s", got)
	}
	e.call(http.MethodPost, "/v1/chat/completions", "", ping)
	e.call(http.MethodPost, "/v1/chat/completions", "tp_no-such-key-5678", ping)
	e.call(http.MethodPost, "/v1/chat/completions", "tp_no-such-key-5678", ping)

	got := e.scrape(t)
	wantMetrics(t, got,
		`transponder_auth_refused_total{reason="missing"} 1`,
		`transponder_auth_refused_total{reason="unknown_key"} 2`,
		`transponder_auth_refused_total{reason="wrong_audience"} 0`,
		`transponder_auth_refused_total{reason="expired_token"} 0`,
	)
	if strings.Contains(got, "tp_no-such-key-5678") {
		t.Error("a refused key is in the metrics")
	}
	for _, reason := range auth.Reasons {
		if !strings.Contains(got, fmt.Sprintf(`transponder_auth_refused_total{reason=%q}`, string(reason))) {
			t.Errorf("reason %q is not in the scrape", reason)
		}
	}
	if up.calls.Load() != 0 {
		t.Errorf("the provider was called %d times", up.calls.Load())
	}
}
