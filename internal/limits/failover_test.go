package limits

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

var errDown = errors.New("connection refused")

// brokenStore fails every call while down is set, and counts in memory otherwise.
type brokenStore struct {
	*MemoryStore
	down bool
}

func (b *brokenStore) Add(ctx context.Context, key string, n, limit int64, w Window, now time.Time) (Result, error) {
	if b.down {
		return Result{}, errDown
	}
	return b.MemoryStore.Add(ctx, key, n, limit, w, now)
}

func (b *brokenStore) Adjust(ctx context.Context, key string, delta int64, w Window, at time.Time) error {
	if b.down {
		return errDown
	}
	return b.MemoryStore.Adjust(ctx, key, delta, w, at)
}

func (b *brokenStore) Used(ctx context.Context, key string, w Window, now time.Time) (int64, error) {
	if b.down {
		return 0, errDown
	}
	return b.MemoryStore.Used(ctx, key, w, now)
}

func (b *brokenStore) Take(ctx context.Context, key string, cost int64, r Rate, now time.Time) (Bucket, error) {
	if b.down {
		return Bucket{}, errDown
	}
	return b.MemoryStore.Take(ctx, key, cost, r, now)
}

func (b *brokenStore) Settle(ctx context.Context, key string, delta int64, r Rate, now time.Time) error {
	if b.down {
		return errDown
	}
	return b.MemoryStore.Settle(ctx, key, delta, r, now)
}

func (b *brokenStore) Acquire(ctx context.Context, key string, limit int64) (bool, func(), error) {
	if b.down {
		return false, func() {}, errDown
	}
	return b.MemoryStore.Acquire(ctx, key, limit)
}

func TestFailoverLocalShare(t *testing.T) {
	shared := &brokenStore{MemoryStore: NewMemoryStore()}
	var changes []error
	f := &Failover{Shared: shared, Local: NewMemoryStore(), Replicas: func() int64 { return 4 },
		OnChange: func(err error) { changes = append(changes, err) }}
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	rate := Rate{N: 8, Per: time.Minute}

	if r, err := f.Add(ctx, "k", 30, 100, Day, now); err != nil || !r.OK || r.Used != 30 {
		t.Fatalf("shared add %+v, %v", r, err)
	}
	shared.down = true
	// A replica on its own gets a quarter of each limit: 25 of 100, 2 of 8 a minute, 1 of 4 slots.
	if r, err := f.Add(ctx, "k", 26, 100, Day, now); err != nil || r.OK {
		t.Fatalf("local add over the share %+v, %v", r, err)
	}
	if r, _ := f.Add(ctx, "k", 25, 100, Day, now); !r.OK || r.Limit != 25 {
		t.Fatalf("local add within the share %+v", r)
	}
	if err := f.Adjust(ctx, "k", -5, Day, now); err != nil {
		t.Fatal(err)
	}
	if u, err := f.Used(ctx, "k", Day, now); err != nil || u != 20 {
		t.Fatalf("local used %d, %v", u, err)
	}
	for i, want := range []bool{true, true, false} {
		if b, err := f.Take(ctx, "r", 1, rate, now); err != nil || b.OK != want {
			t.Fatalf("take %d: %+v, %v", i, b, err)
		}
	}
	if err := f.Settle(ctx, "r", -2, rate, now); err != nil {
		t.Fatal(err)
	}
	if ok, release, err := f.Acquire(ctx, "c", 4); err != nil || !ok {
		t.Fatalf("first local slot %v, %v", ok, err)
	} else if ok2, _, _ := f.Acquire(ctx, "c", 4); ok2 {
		t.Fatal("a second local slot passed a share of one")
	} else {
		release()
	}
	shared.down = false
	if u, err := f.Used(ctx, "k", Day, now); err != nil || u != 30 {
		t.Fatalf("shared used after the outage %d, %v", u, err)
	}
	if len(changes) != 2 || !errors.Is(changes[0], errDown) || changes[1] != nil {
		t.Fatalf("changes %v, want one failure and one recovery", changes)
	}
}

func TestFailoverDeny(t *testing.T) {
	shared := &brokenStore{MemoryStore: NewMemoryStore(), down: true}
	f := &Failover{Shared: shared, Local: NewMemoryStore(), Deny: true}
	now := time.Now()
	checks := map[string]error{}
	_, checks["add"] = f.Add(ctx, "k", 1, 10, Day, now)
	checks["adjust"] = f.Adjust(ctx, "k", 1, Day, now)
	_, checks["used"] = f.Used(ctx, "k", Day, now)
	_, checks["take"] = f.Take(ctx, "k", 1, Rate{N: 1, Per: time.Second}, now)
	checks["settle"] = f.Settle(ctx, "k", 1, Rate{N: 1, Per: time.Second}, now)
	_, _, checks["acquire"] = f.Acquire(ctx, "k", 1)
	for name, err := range checks {
		if !errors.Is(err, ErrStore) || !errors.Is(err, errDown) {
			t.Errorf("%s: err = %v, want ErrStore wrapping the cause", name, err)
		}
	}
}

// A reservation is settled in the store that took it, so a call that spans the shared store
// going down or coming back neither takes spend off the shared count nor off another call's
// local reservation.
func TestFailoverSettlesWhereReserved(t *testing.T) {
	budget := Config{Budgets: []Budget{{Subject: "user:*", Window: Month, LimitNUSD: 10e9}}}
	rate := Rate{N: 1_000_000, Per: time.Minute}
	cfg := budget
	cfg.DefaultProfile = "p"
	cfg.Profiles = []Profile{profile("p", func(p *Profile) { p.TokenRate = &rate })}
	setup := func() (*Limiter, *brokenStore, *Failover) {
		shared := &brokenStore{MemoryStore: NewMemoryStore()}
		f := &Failover{Shared: shared, Local: NewMemoryStore()}
		return New(cfg, f, func() time.Time { return t0 }), shared, f
	}
	spend := func(t *testing.T, l *Limiter, est, used int64) *Ticket {
		t.Helper()
		tk, d := l.Admit(ctx, alice, Estimate{InputTokens: 10, CostNUSD: est})
		if d != nil {
			t.Fatalf("denied %+v", d)
		}
		if used >= 0 {
			tk.Settle(10, used)
			tk.Release()
		}
		return tk
	}

	t.Run("reserved during an outage, settled after it", func(t *testing.T) {
		l, shared, _ := setup()
		spend(t, l, 5e9, 5e9)
		shared.down = true
		tk := spend(t, l, 2e9, -1)
		shared.down = false
		tk.Settle(10, 0.1e9)
		tk.Release()
		if u, _ := shared.Used(ctx, "usd|user:alice", Month, t0); u < 5e9 {
			t.Fatalf("shared spend %d after the call, want at least the 5e9 spent before it", u)
		}
	})
	t.Run("reserved before an outage, settled during it", func(t *testing.T) {
		l, shared, f := setup()
		shared.down = true
		other := spend(t, l, 1e9, -1)
		shared.down = false
		tk := spend(t, l, 3e9, -1)
		shared.down = true
		tk.Settle(10, 0)
		tk.Release()
		if u, _ := f.Local.Used(ctx, "usd|user:alice", Month, t0); u < 1e9 {
			t.Fatalf("local spend %d, want the other call's 1e9 still reserved", u)
		}
		other.Release()
	})
}

// A caller that gives up while the shared store is asked is not an outage.
func TestFailoverCancelledCaller(t *testing.T) {
	var changes []error
	f := &Failover{Shared: ctxStore{NewMemoryStore()}, Local: NewMemoryStore(), OnChange: func(err error) { changes = append(changes, err) }}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := f.Add(gone, "k", 1, 10, Day, t0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Add for a caller that left: %v", err)
	}
	if _, _, err := f.Acquire(gone, "c", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire for a caller that left: %v", err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes %v, want none", changes)
	}
	if u, _ := f.Local.Used(ctx, "k", Day, t0); u != 0 {
		t.Fatalf("counted %d locally for a caller that left", u)
	}
}

// ctxStore fails a call whose context has ended, as pgx does.
type ctxStore struct{ *MemoryStore }

func (s ctxStore) Add(ctx context.Context, key string, n, limit int64, w Window, now time.Time) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return s.MemoryStore.Add(ctx, key, n, limit, w, now)
}

func (s ctxStore) Acquire(ctx context.Context, key string, limit int64) (bool, func(), error) {
	if err := ctx.Err(); err != nil {
		return false, func() {}, err
	}
	return s.MemoryStore.Acquire(ctx, key, limit)
}

func TestAdmitWhenTheStoreFails(t *testing.T) {
	r := Rate{N: 10, Per: time.Minute}
	tests := []struct {
		name string
		p    func(*Profile)
		cfg  Config
	}{
		{"concurrency", func(p *Profile) { p.Concurrency = 2 }, Config{}},
		{"request rate", func(p *Profile) { p.RequestRate = &r }, Config{}},
		{"token rate", func(p *Profile) { p.TokenRate = &Rate{N: 100000, Per: time.Minute} }, Config{}},
		{"allowance", func(p *Profile) { p.TokensPerDay = 100000 }, Config{}},
		{"budget", func(*Profile) {}, Config{Budgets: []Budget{{Subject: "user:*", Window: Month, LimitNUSD: 1e9}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			cfg.DefaultProfile = "p"
			cfg.Profiles = []Profile{profile("p", tt.p)}
			shared := &brokenStore{MemoryStore: NewMemoryStore(), down: true}
			l := New(cfg, &Failover{Shared: shared, Local: NewMemoryStore(), Deny: true}, func() time.Time { return t0 })
			_, d := l.Admit(ctx, alice, Estimate{InputTokens: 10})
			if d == nil || d.Status != http.StatusServiceUnavailable || d.Code != "overloaded" || d.RetryAfter != time.Second {
				t.Fatalf("denial %+v", d)
			}
			shared.down = false
			if _, d := l.Admit(ctx, alice, Estimate{InputTokens: 10}); d != nil {
				t.Fatalf("a refused call kept something: %+v", d)
			}
		})
	}
}
