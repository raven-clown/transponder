package limits

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

func limiter(t *testing.T, cfg Config) *Limiter {
	t.Helper()
	return New(cfg, NewMemoryStore(), func() time.Time { return t0 })
}

func profile(_ string, edit func(*Profile)) Profile {
	p := Profile{Name: "p"}
	edit(&p)
	return p
}

var alice = Caller{User: "alice", Teams: []string{"platform"}, Key: "alice-laptop"}

func mustAdmit(t *testing.T, l *Limiter, c Caller, e Estimate) *Ticket {
	t.Helper()
	tk, d := l.Admit(ctx, c, e)
	if d != nil {
		t.Fatalf("denied: %+v", d)
	}
	return tk
}

func TestNoLimits(t *testing.T) {
	l := limiter(t, Config{})
	tk := mustAdmit(t, l, alice, Estimate{InputTokens: 1 << 40, OutputTokens: 1 << 40, CostNUSD: 1 << 60})
	h := http.Header{}
	tk.SetHeaders(h)
	if len(h) != 0 {
		t.Fatalf("headers %v", h)
	}
	tk.Settle(10, 10)
	tk.Release()
}

func TestRequestRate(t *testing.T) {
	r := Rate{N: 2, Per: time.Minute}
	l := limiter(t, Config{DefaultProfile: "p", Profiles: []Profile{profile("p", func(p *Profile) { p.RequestRate = &r })}})
	for range 2 {
		mustAdmit(t, l, alice, Estimate{}).Release()
	}
	_, d := l.Admit(ctx, alice, Estimate{})
	if d == nil || d.Status != 429 || d.Code != "rate_limited" || d.RetryAfter != 30*time.Second {
		t.Fatalf("denial %+v", d)
	}
	if _, d := l.Admit(ctx, Caller{User: "bob"}, Estimate{}); d != nil {
		t.Fatalf("bob shares alice's rate: %+v", d)
	}
	tk := mustAdmit(t, limiter(t, Config{DefaultProfile: "p", Profiles: []Profile{profile("p", func(p *Profile) { p.RequestRate = &Rate{N: 60, Per: time.Minute} })}}), alice, Estimate{})
	h := http.Header{}
	tk.SetHeaders(h)
	if h.Get("RateLimit-Policy") != `"requests";q=60;w=60` || h.Get("RateLimit") != `"requests";r=59;t=1` {
		t.Fatalf("headers %v", h)
	}
}

func TestConcurrency(t *testing.T) {
	l := limiter(t, Config{DefaultProfile: "p", Profiles: []Profile{profile("p", func(p *Profile) { p.Concurrency = 1 })}})
	first := mustAdmit(t, l, alice, Estimate{})
	_, d := l.Admit(ctx, alice, Estimate{})
	if d == nil || d.Status != 429 || d.Code != "concurrency_limited" {
		t.Fatalf("denial %+v", d)
	}
	first.Release()
	mustAdmit(t, l, alice, Estimate{})
}

func TestInputTooLarge(t *testing.T) {
	l := limiter(t, Config{DefaultProfile: "p", Profiles: []Profile{profile("p", func(p *Profile) {
		p.MaxInputTokens, p.MaxOutputTokens, p.MaxReasoningTokens, p.Concurrency = 100, 50, 20, 1
	})}})
	_, d := l.Admit(ctx, alice, Estimate{InputTokens: 101})
	if d == nil || d.Status != 400 || d.Code != "input_too_large" {
		t.Fatalf("denial %+v", d)
	}
	tk := mustAdmit(t, l, alice, Estimate{InputTokens: 100})
	if out, reasoning := tk.Caps(); out != 50 || reasoning != 20 {
		t.Fatalf("caps %d %d", out, reasoning)
	}
}

func TestTokenAllowance(t *testing.T) {
	l := limiter(t, Config{DefaultProfile: "p", Profiles: []Profile{profile("p", func(p *Profile) { p.TokensPerDay = 1000 })}})
	first := mustAdmit(t, l, alice, Estimate{InputTokens: 100, OutputTokens: 500})
	_, d := l.Admit(ctx, alice, Estimate{InputTokens: 100, OutputTokens: 500})
	if d == nil || d.Status != 402 || d.Code != "quota_exceeded" || !d.Reset.Equal(t0.Add(14*time.Hour)) {
		t.Fatalf("denial %+v", d)
	}
	first.Settle(150, 0)
	second := mustAdmit(t, l, alice, Estimate{InputTokens: 100, OutputTokens: 500})
	second.Release()
	third := mustAdmit(t, l, alice, Estimate{InputTokens: 100, OutputTokens: 500})
	third.Settle(850, 0)
	if _, d := l.Admit(ctx, alice, Estimate{InputTokens: 1}); d == nil {
		t.Fatal("admitted past a spent allowance")
	}
}

func TestTokenRate(t *testing.T) {
	r := Rate{N: 1000, Per: time.Minute}
	l := limiter(t, Config{DefaultProfile: "p", Profiles: []Profile{profile("p", func(p *Profile) { p.TokenRate = &r })}})
	big := mustAdmit(t, l, alice, Estimate{InputTokens: 100, OutputTokens: 32000})
	_, d := l.Admit(ctx, alice, Estimate{InputTokens: 9, OutputTokens: 1})
	if d == nil || d.Status != 429 || d.Code != "rate_limited" {
		t.Fatalf("denial %+v", d)
	}
	big.Settle(200, 0)
	mustAdmit(t, l, alice, Estimate{InputTokens: 9, OutputTokens: 1})
}

func TestBudgets(t *testing.T) {
	cfg := Config{Budgets: []Budget{
		{Subject: "user:*", Window: Month, LimitNUSD: 1_000_000_000},
		{Subject: "user:bob", Window: Month, LimitNUSD: 10_000_000_000},
		{Subject: "team:platform", Window: Day, LimitNUSD: 5_000_000_000},
	}}
	l := limiter(t, cfg)
	mustAdmit(t, l, alice, Estimate{CostNUSD: 600_000_000})
	_, d := l.Admit(ctx, alice, Estimate{CostNUSD: 600_000_000})
	if d == nil || d.Status != 402 || d.Code != "budget_exceeded" || d.Subject != "user:alice" ||
		!strings.Contains(d.Message, "1.00 USD monthly") || !d.Reset.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("denial %+v", d)
	}
	bob := Caller{User: "bob", Teams: []string{"platform"}}
	mustAdmit(t, l, bob, Estimate{CostNUSD: 4_000_000_000})
	_, d = l.Admit(ctx, bob, Estimate{CostNUSD: 1_000_000_000})
	if d == nil || d.Subject != "team:platform" {
		t.Fatalf("team budget not applied: %+v", d)
	}
	keyed := limiter(t, Config{Budgets: []Budget{{Subject: "key:ci", Window: Day, LimitNUSD: 1}}})
	if _, d := keyed.Admit(ctx, Caller{Key: "ci"}, Estimate{CostNUSD: 2}); d == nil || d.Subject != "key:ci" {
		t.Fatalf("key budget not applied: %+v", d)
	}
}

func TestDeniedRollsBack(t *testing.T) {
	l := limiter(t, Config{
		DefaultProfile: "p",
		Profiles:       []Profile{profile("p", func(p *Profile) { p.TokensPerDay, p.Concurrency = 1000, 1 })},
		Budgets:        []Budget{{Subject: "user:*", Window: Day, LimitNUSD: 10}},
	})
	if _, d := l.Admit(ctx, alice, Estimate{InputTokens: 899, OutputTokens: 1, CostNUSD: 11}); d == nil || d.Code != "budget_exceeded" {
		t.Fatalf("denial %+v", d)
	}
	mustAdmit(t, l, alice, Estimate{InputTokens: 999, OutputTokens: 1, CostNUSD: 10})
}

func TestProfilePrecedence(t *testing.T) {
	names := []string{"default", "team", "user", "key"}
	var profiles []Profile
	for _, n := range names {
		profiles = append(profiles, Profile{Name: n})
	}
	cfg := Config{Profiles: profiles, DefaultProfile: "default", Bindings: []Binding{
		{Subject: "team:platform", Profile: "team"}, {Subject: "user:alice", Profile: "user"}, {Subject: "key:alice-laptop", Profile: "key"},
	}}
	l := limiter(t, cfg)
	for _, tt := range []struct {
		c    Caller
		want string
	}{
		{alice, "key"},
		{Caller{User: "alice", Teams: []string{"platform"}, Key: "other"}, "user"},
		{Caller{User: "carol", Teams: []string{"platform"}}, "team"},
		{Caller{User: "dave"}, "default"},
	} {
		p, ok := l.Profile(tt.c)
		if !ok || p.Name != tt.want {
			t.Errorf("Profile(%+v) = %q", tt.c, p.Name)
		}
	}
	if _, ok := limiter(t, Config{}).Profile(alice); ok {
		t.Fatal("profile with no config")
	}
}

func TestSeed(t *testing.T) {
	l := limiter(t, Config{
		DefaultProfile: "p",
		Profiles:       []Profile{profile("p", func(p *Profile) { p.TokensPerMonth = 1000 })},
		Budgets:        []Budget{{Subject: "team:platform", Window: Month, LimitNUSD: 100}},
	})
	l.Seed(ctx, alice, t0.Add(-time.Hour), 990, 95)
	l.Seed(ctx, alice, t0.AddDate(0, -1, 0), 1_000_000, 1_000_000)
	if _, d := l.Admit(ctx, alice, Estimate{InputTokens: 20}); d == nil || d.Code != "quota_exceeded" {
		t.Fatalf("token denial %+v", d)
	}
	if _, d := l.Admit(ctx, alice, Estimate{InputTokens: 4, OutputTokens: 1, CostNUSD: 6}); d == nil || d.Subject != "team:platform" {
		t.Fatalf("budget denial %+v", d)
	}
}

func TestNilTicketCaps(t *testing.T) {
	var tk *Ticket
	if out, r := tk.Caps(); out != 0 || r != 0 {
		t.Fatal("nil ticket has caps")
	}
}

func TestCostPricedAfterTheCap(t *testing.T) {
	cfg := Config{DefaultProfile: "p", Profiles: []Profile{profile("p", func(p *Profile) { p.MaxOutputTokens, p.USDPerMonth = 1000, 1_000_000_000 })}}
	l := limiter(t, cfg)
	price := func(_, out int64) int64 { return out * 10_000 }
	tk := mustAdmit(t, l, alice, Estimate{OutputTokens: 200000, Price: price})
	tk.Release()
	if _, d := l.Admit(ctx, alice, Estimate{OutputTokens: 1 << 62, Price: price}); d != nil {
		t.Fatalf("huge max_tokens with a cap denied: %+v", d)
	}
}

func TestDefaultOutputReservation(t *testing.T) {
	l := limiter(t, Config{DefaultProfile: "p", Profiles: []Profile{profile("p", func(p *Profile) { p.TokensPerDay = DefaultOutputReservation + 100 })}})
	mustAdmit(t, l, alice, Estimate{InputTokens: 10})
	if _, d := l.Admit(ctx, alice, Estimate{InputTokens: 10}); d == nil {
		t.Fatal("calls with no max_tokens reserved no output")
	}
}

func TestExactUserReplacesPerWindow(t *testing.T) {
	l := limiter(t, Config{Budgets: []Budget{
		{Subject: "user:*", Window: Day, LimitNUSD: 1_000_000_000},
		{Subject: "user:alice", Window: Month, LimitNUSD: 100_000_000_000},
	}})
	if _, d := l.Admit(ctx, alice, Estimate{CostNUSD: 50_000_000_000}); d == nil || d.Subject != "user:alice" {
		t.Fatalf("the daily user:* cap was dropped: %+v", d)
	}
}

func TestKeep(t *testing.T) {
	l := limiter(t, Config{DefaultProfile: "p", Profiles: []Profile{profile("p", func(p *Profile) { p.TokensPerDay = 1000 })}})
	tk := mustAdmit(t, l, alice, Estimate{InputTokens: 599, OutputTokens: 1})
	tk.Keep()
	tk.Release()
	if _, d := l.Admit(ctx, alice, Estimate{InputTokens: 599, OutputTokens: 1}); d == nil {
		t.Fatal("a kept reservation was given back")
	}
}
