package server_test

import (
	"context"
	"encoding/json"
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
)

func setupLimited(t *testing.T, routes []router.Route, cfg limits.Config) *env {
	t.Helper()
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	plain, hash, last4, err := auth.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.CreateKey(ctx, store.APIKey{OrgID: store.DefaultOrg, Name: "alice", Hash: hash, Last4: last4, Role: string(auth.RoleMember), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(server.Deps{
		Auth:    auth.NewAuthenticator(db, time.Now),
		Router:  router.New(routes),
		Prices:  pricing.Default(),
		Usage:   db,
		Logger:  slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Now:     time.Now,
		Limiter: limits.New(cfg, limits.NewMemoryStore(), time.Now),
	}))
	t.Cleanup(srv.Close)
	return &env{srv: srv, db: db, key: plain}
}

func oneProfile(edit func(*limits.Profile)) limits.Config {
	p := limits.Profile{Name: "p"}
	edit(&p)
	return limits.Config{Profiles: []limits.Profile{p}, DefaultProfile: "p"}
}

func TestBudgetDenied(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":1000000,"completion_tokens":0}}`)
	})
	cfg := limits.Config{Budgets: []limits.Budget{{Subject: "user:*", Window: limits.Month, LimitNUSD: 100_000_000}}}
	e := setupLimited(t, []router.Route{openaiRoute("*", "gpt-4o-mini", up)}, cfg)
	if res := e.post(t, ping); res.status != 200 {
		t.Fatalf("first call %d: %s", res.status, res.body)
	}
	res := e.post(t, ping)
	if res.status != http.StatusPaymentRequired || errorCode(t, res) != "budget_exceeded" || !strings.Contains(string(res.body), "0.10 USD monthly") {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	res = e.messages(t, nil, `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`)
	if res.status != http.StatusPaymentRequired || anthropicErrorType(t, res) != "billing_error" {
		t.Fatalf("messages %d: %s", res.status, res.body)
	}
	if n := len(e.usage(t)); n != 1 {
		t.Fatalf("denied calls were recorded: %d records", n)
	}
}

func TestRequestRateDenied(t *testing.T) {
	r := limits.Rate{N: 1, Per: time.Minute}
	e := setupLimited(t, echoRoutes(echo.New("echo")), oneProfile(func(p *limits.Profile) { p.RequestRate = &r }))
	res := e.post(t, ping)
	if res.status != 200 || res.header.Get("RateLimit-Policy") != `"requests";q=1;w=60` || res.header.Get("RateLimit") == "" {
		t.Fatalf("first call %d, headers %v", res.status, res.header)
	}
	res = e.post(t, ping)
	if res.status != http.StatusTooManyRequests || errorCode(t, res) != "rate_limited" || res.header.Get("Retry-After") != "60" {
		t.Fatalf("status %d, headers %v: %s", res.status, res.header, res.body)
	}
}

func TestInputTooLarge(t *testing.T) {
	e := setupLimited(t, echoRoutes(echo.New("echo")), oneProfile(func(p *limits.Profile) { p.MaxInputTokens = 10 }))
	res := e.post(t, `{"model":"m","messages":[{"role":"user","content":"`+strings.Repeat("สวัสดี", 3)+`"}]}`)
	if res.status != http.StatusBadRequest || errorCode(t, res) != "input_too_large" {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
}

func TestOutputCaps(t *testing.T) {
	var got map[string]json.RawMessage
	capture := func(body string) *httptest.Server {
		return upstream(t, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			got = nil
			_ = json.Unmarshal(b, &got)
			_, _ = io.WriteString(w, body)
		})
	}
	cfg := oneProfile(func(p *limits.Profile) { p.MaxOutputTokens, p.MaxReasoningTokens = 100, 50 })
	oaBody := `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`

	e := setupLimited(t, []router.Route{openaiRoute("*", "", capture(oaBody))}, cfg)
	e.post(t, `{"model":"m","max_tokens":5000,"messages":[{"role":"user","content":"x"}]}`)
	if string(got["max_tokens"]) != "100" {
		t.Fatalf("max_tokens not lowered: %v", got)
	}
	e.post(t, `{"model":"m","max_tokens":20,"messages":[{"role":"user","content":"x"}]}`)
	if string(got["max_tokens"]) != "20" {
		t.Fatalf("max_tokens raised: %v", got)
	}
	e.post(t, ping)
	if string(got["max_completion_tokens"]) != "100" {
		t.Fatalf("OpenAI's limit not set when missing: %v", got)
	}

	an := capture(`{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	e = setupLimited(t, []router.Route{anthropicRoute("*", "", an)}, oneProfile(func(p *limits.Profile) { p.MaxOutputTokens, p.MaxReasoningTokens = 100, 2000 }))
	e.messages(t, nil, `{"model":"m","max_tokens":4000,"thinking":{"type":"enabled","budget_tokens":3000},"messages":[{"role":"user","content":"x"}]}`)
	var thinking struct {
		BudgetTokens int `json:"budget_tokens"`
	}
	_ = json.Unmarshal(got["thinking"], &thinking)
	if string(got["max_tokens"]) != "100" || got["thinking"] != nil || thinking.BudgetTokens != 0 {
		t.Fatalf("anthropic caps not applied: %v", got)
	}
}

func TestSettleAndRelease(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":900,"completion_tokens":0}}`)
	})
	cfg := oneProfile(func(p *limits.Profile) { p.TokensPerDay, p.Concurrency = 1000, 1 })
	e := setupLimited(t, []router.Route{openaiRoute("ok", "", up), {Match: "echo", Targets: []router.Target{{Provider: echo.New("echo"), Type: "echo"}}}}, cfg)
	if res := e.post(t, `{"model":"echo","max_tokens":1,"messages":[{"role":"user","content":[{"type":"image_url"}]}]}`); res.status != 400 {
		t.Fatalf("bad request %d: %s", res.status, res.body)
	}
	if res := e.post(t, `{"model":"ok","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`); res.status != 200 {
		t.Fatalf("slot or reservation not given back: %d %s", res.status, res.body)
	}
	res := e.post(t, `{"model":"ok","max_tokens":200,"messages":[{"role":"user","content":"hi"}]}`)
	if res.status != http.StatusPaymentRequired || errorCode(t, res) != "quota_exceeded" {
		t.Fatalf("real usage not settled: %d %s", res.status, res.body)
	}
}

func TestInputEstimateCountsTools(t *testing.T) {
	e := setupLimited(t, echoRoutes(echo.New("echo")), oneProfile(func(p *limits.Profile) { p.MaxInputTokens = 100 }))
	big := strings.Repeat("describe ", 200)
	res := e.post(t, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","description":"`+big+`"}}]}`)
	if res.status != http.StatusBadRequest || errorCode(t, res) != "input_too_large" {
		t.Fatalf("a large tool description slipped past: %d %s", res.status, res.body)
	}
	res, err := e.doPath(http.MethodPost, "/v1/embeddings", e.auth(), `{"model":"m","input":[[`+strings.Repeat("1,", 200)+`1]]}`)
	if err != nil {
		t.Fatal(err)
	}
	if res.status != http.StatusBadRequest || errorCode(t, res) != "input_too_large" {
		t.Fatalf("token-array embeddings slipped past: %d %s", res.status, res.body)
	}
}

func TestCapsDropCaseVariants(t *testing.T) {
	var got map[string]json.RawMessage
	up := upstream(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	e := setupLimited(t, []router.Route{anthropicRoute("*", "", up)}, oneProfile(func(p *limits.Profile) { p.MaxOutputTokens = 2048 }))
	e.messages(t, nil, `{"model":"m","max_tokens":16000,"Max_Tokens":99999,"thinking":{"type":"enabled","budget_tokens":8000},"messages":[{"role":"user","content":"x"}]}`)
	if _, ok := got["Max_Tokens"]; ok {
		t.Fatalf("case variant reached the provider: %v", got)
	}
	var thinking struct {
		BudgetTokens int `json:"budget_tokens"`
	}
	_ = json.Unmarshal(got["thinking"], &thinking)
	if string(got["max_tokens"]) != "2048" || thinking.BudgetTokens != 2047 {
		t.Fatalf("thinking budget not kept under max_tokens: %v", got)
	}
	e.messages(t, nil, `{"model":"m","max_tokens":16000,"thinking":{"type":"enabled","budget_tokens":8000},"messages":[{"role":"user","content":"x"}]}`)
	e = setupLimited(t, []router.Route{anthropicRoute("*", "", up)}, oneProfile(func(p *limits.Profile) { p.MaxOutputTokens = 900 }))
	e.messages(t, nil, `{"model":"m","max_tokens":16000,"thinking":{"type":"enabled","budget_tokens":8000},"messages":[{"role":"user","content":"x"}]}`)
	if _, ok := got["thinking"]; ok {
		t.Fatalf("a thinking budget under 1024 was sent: %v", got)
	}
}

func TestNChoicesReserveMore(t *testing.T) {
	up := upstream(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	e := setupLimited(t, []router.Route{openaiRoute("*", "", up)}, oneProfile(func(p *limits.Profile) { p.TokensPerDay = 1000 }))
	res := e.post(t, `{"model":"m","n":5,"max_tokens":300,"messages":[{"role":"user","content":"x"}]}`)
	if res.status != http.StatusPaymentRequired {
		t.Fatalf("n=5 at 300 tokens each passed a 1000-token allowance: %d", res.status)
	}
}
