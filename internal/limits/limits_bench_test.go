package limits_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/limits"
)

// benchLimiter checks requests, tokens, concurrency, a daily cost cap and a team budget, so a
// call touches every kind of counter.
func benchLimiter(b *testing.B) *limits.Limiter {
	p, err := limits.ParseProfile("bench", map[string]any{
		"requests":     map[string]any{"rate": "10000000/m", "concurrency": 100000},
		"llm.tokens":   map[string]any{"rate": "10000000000/m", "per_day": 1000000000000},
		"llm.cost_usd": map[string]any{"per_day": 1000000},
	})
	if err != nil {
		b.Fatal(err)
	}
	budget, err := limits.ParseBudget("team:platform", "monthly", 1000000)
	if err != nil {
		b.Fatal(err)
	}
	return limits.New(limits.Config{Profiles: []limits.Profile{p}, DefaultProfile: "bench", Budgets: []limits.Budget{budget}}, limits.NewMemoryStore(), time.Now)
}

// BenchmarkAdmitSettle is the limits work of one call: reserve before it, settle after it.
func BenchmarkAdmitSettle(b *testing.B) {
	l := benchLimiter(b)
	ctx := context.Background()
	caller := limits.Caller{User: "ana", Teams: []string{"platform"}, Key: "bench"}
	est := limits.Estimate{InputTokens: 420, OutputTokens: 512, CostNUSD: 900_000}
	b.ReportAllocs()
	for b.Loop() {
		t, d := l.Admit(ctx, caller, est)
		if d != nil {
			b.Fatalf("denied: %+v", d)
		}
		t.Settle(676, 650_000)
		t.Release()
	}
}

// BenchmarkAdmitSettleParallel is the same from many callers at once, half of them the same user.
func BenchmarkAdmitSettleParallel(b *testing.B) {
	l := benchLimiter(b)
	ctx := context.Background()
	est := limits.Estimate{InputTokens: 420, OutputTokens: 512, CostNUSD: 900_000}
	b.ReportAllocs()
	var n atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		caller := limits.Caller{User: fmt.Sprintf("user-%d", n.Add(1)%2), Teams: []string{"platform"}, Key: "bench"}
		for pb.Next() {
			t, d := l.Admit(ctx, caller, est)
			if d != nil {
				b.Errorf("denied: %+v", d)
				return
			}
			t.Settle(676, 650_000)
			t.Release()
		}
	})
}
