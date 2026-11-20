package postgres

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/limits/limitstest"
	"github.com/raven-clown/transponder/internal/store/postgres/pgtest"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

func counters(t *testing.T, db *DB) *Counters {
	t.Helper()
	c, err := db.Counters(context.Background(), quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestCountersStore(t *testing.T) {
	pgtest.DSN(t)
	limitstest.Run(t, func(t *testing.T) limits.Store { return counters(t, openTemp(t)) })
}

// Two replicas, each with its own limiter and its own connections, admit calls against one
// budget at the same time. Every call reserves more than it ends up costing, as calls do, and
// the total settled must stay within the budget.
func TestTwoLimitersShareABudget(t *testing.T) {
	dsn := pgtest.DSN(t)
	now := func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }
	const (
		budget  = int64(1_000_000_000) // 1 USD
		reserve = int64(30_000_000)
		cost    = int64(25_000_000)
	)
	cfg := limits.Config{Budgets: []limits.Budget{{Subject: "team:ml", Window: limits.Month, LimitNUSD: budget}}}
	replicas := []*limits.Limiter{
		limits.New(cfg, counters(t, openAt(t, dsn)), now),
		limits.New(cfg, counters(t, openAt(t, dsn)), now),
	}
	var spent, admitted atomic.Int64
	var wg sync.WaitGroup
	for i := range 200 {
		l := replicas[i%2]
		caller := limits.Caller{User: "u" + string(rune('a'+i%7)), Teams: []string{"ml"}}
		wg.Go(func() {
			tk, d := l.Admit(context.Background(), caller, limits.Estimate{CostNUSD: reserve, NoOutput: true})
			if d != nil {
				if d.Code != "budget_exceeded" {
					t.Errorf("denial %+v", d)
				}
				return
			}
			admitted.Add(1)
			spent.Add(cost)
			tk.Settle(1, cost)
			tk.Release()
		})
	}
	wg.Wait()
	if spent.Load() > budget {
		t.Fatalf("spent %d nano-USD over a budget of %d across %d calls", spent.Load(), budget, admitted.Load())
	}
	// The last call that fit left room for less than one more reservation.
	if left := budget - spent.Load(); left >= reserve {
		t.Fatalf("only %d calls admitted, %d nano-USD left unused", admitted.Load(), left)
	}
	for _, l := range replicas {
		st := l.Status(context.Background(), limits.Caller{User: "ua", Teams: []string{"ml"}})
		if len(st) != 1 || st[0].Used != spent.Load() {
			t.Fatalf("status %+v, want %d used on every replica", st, spent.Load())
		}
	}
}

// A replica that died holding a slot frees it once the lease runs out.
func TestDeadReplicaSlotExpires(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := counters(t, openAt(t, dsn)), counters(t, openAt(t, dsn))
	ctx := context.Background()
	if ok, _, err := a.Acquire(ctx, "conc|user:x", 1); !ok || err != nil {
		t.Fatalf("acquire %v %v", ok, err)
	}
	if ok, _, _ := b.Acquire(ctx, "conc|user:x", 1); ok {
		t.Fatal("the slot was taken twice")
	}
	if _, err := a.pool.Exec(ctx, `UPDATE limit_slots SET expires_at = now() - interval '1 second' WHERE replica = $1`, a.replica); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := b.Acquire(ctx, "conc|user:x", 1); !ok || err != nil {
		t.Fatalf("an expired slot still counts: %v %v", ok, err)
	}
}

func TestHeartbeat(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := counters(t, openAt(t, dsn)), counters(t, openAt(t, dsn))
	ctx := context.Background()
	if err := a.heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if n := a.Replicas(); n != 2 {
		t.Fatalf("replicas %d, want 2", n)
	}

	ok, release, err := a.Acquire(ctx, "conc|k", 5)
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	// A slot whose release could not reach the database is gone after the next heartbeat.
	var id int64
	for held := range a.slots {
		id = held
	}
	a.giveBack(id)
	if err := a.heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	var left int
	if err := a.pool.QueryRow(ctx, `SELECT count(*) FROM limit_slots`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("slots left %d, %v", left, err)
	}
	release()

	if len(a.released) != 0 {
		t.Fatalf("released %v, want it emptied once the delete went through", a.released)
	}

	// A held slot is renewed.
	if ok, _, _ := a.Acquire(ctx, "conc|k", 5); !ok {
		t.Fatal("acquire")
	}
	if _, err := a.pool.Exec(ctx, `UPDATE limit_slots SET expires_at = now() + interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if err := a.heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	var renewed bool
	if err := a.pool.QueryRow(ctx, `SELECT bool_and(expires_at > now() + interval '20 seconds') FROM limit_slots`).Scan(&renewed); err != nil || !renewed {
		t.Fatalf("slot not renewed: %v %v", renewed, err)
	}

	// A replica that closes takes its slots and its place with it.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := a.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM limit_slots) + (SELECT count(*) FROM limit_replicas)`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left %d, %v", left, err)
	}
}

// A slot taken after the heartbeat copied the held slots, and so missing from that copy, must
// survive the heartbeat: its call is still running.
func TestHeartbeatKeepsSlotsItDidNotSee(t *testing.T) {
	a := counters(t, openTemp(t))
	ctx := context.Background()
	if ok, _, err := a.Acquire(ctx, "conc|k", 1); !ok || err != nil {
		t.Fatal(ok, err)
	}
	a.mu.Lock()
	clear(a.slots) // as if Acquire committed between the copy and the batch
	a.mu.Unlock()
	if err := a.heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if ok, _, _ := a.Acquire(ctx, "conc|k", 1); ok {
		t.Fatal("the heartbeat deleted a slot in use, so a second call passed a cap of one")
	}
}

func TestHeartbeatLoop(t *testing.T) {
	old := heartbeatEvery
	heartbeatEvery = 10 * time.Millisecond
	t.Cleanup(func() { heartbeatEvery = old })
	dsn := pgtest.DSN(t)
	a := counters(t, openAt(t, dsn))
	counters(t, openAt(t, dsn))
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(5 * time.Second)
	for a.Replicas() != 2 {
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatalf("replicas %d, want 2 after the loop ran", a.Replicas())
		}
	}
}

func TestSeedOnce(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	build := func() (*limits.MemoryStore, error) { //nolint:unparam // SeedOnce takes a builder that can fail
		m := limits.NewMemoryStore()
		_ = m.Adjust(ctx, "usd|user:a", 700, limits.Month, now)
		return m, nil
	}
	a, b := counters(t, openAt(t, dsn)), counters(t, openAt(t, dsn))
	var seeded atomic.Int64
	var wg sync.WaitGroup
	for _, c := range []*Counters{a, b, a, b} {
		wg.Go(func() {
			did, err := c.SeedOnce(ctx, build)
			if err != nil {
				t.Error(err)
			}
			if did {
				seeded.Add(1)
			}
		})
	}
	wg.Wait()
	if seeded.Load() != 1 {
		t.Fatalf("seeded %d times, want once", seeded.Load())
	}
	if u, _ := b.Used(ctx, "usd|user:a", limits.Month, now); u != 700 {
		t.Fatalf("used %d, want 700", u)
	}
	boom := errors.New("boom")
	c := counters(t, openTemp(t))
	if _, err := c.SeedOnce(ctx, func() (*limits.MemoryStore, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if did, err := c.SeedOnce(ctx, build); !did || err != nil {
		t.Fatalf("a failed seed blocked the next: %v %v", did, err)
	}
}

func TestCountersWhenDown(t *testing.T) {
	db := openTemp(t)
	c := counters(t, db)
	db.pool.Close()
	ctx := context.Background()
	now := time.Now()
	r := limits.Rate{N: 1, Per: time.Second}
	errs := map[string]error{}
	_, errs["add"] = c.Add(ctx, "k", 1, 10, limits.Day, now)
	errs["adjust"] = c.Adjust(ctx, "k", 1, limits.Day, now)
	_, errs["used"] = c.Used(ctx, "k", limits.Day, now)
	_, errs["take"] = c.Take(ctx, "k", 1, r, now)
	errs["settle"] = c.Settle(ctx, "k", 1, r, now)
	_, _, errs["acquire"] = c.Acquire(ctx, "k", 1)
	for name, err := range errs {
		if !errors.Is(err, limits.ErrStore) {
			t.Errorf("%s: err = %v, want ErrStore", name, err)
		}
	}
	if _, err := c.SeedOnce(ctx, nil); err == nil {
		t.Error("SeedOnce on a closed pool succeeded")
	}
}
