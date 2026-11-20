// Package limitstest is the test suite every limits.Store must pass, so the memory counters and
// the shared ones behave the same.
package limitstest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/limits"
)

// Opener returns an empty store that lives until the test ends.
type Opener func(t *testing.T) limits.Store

// Run runs every test of the suite against stores from open.
func Run(t *testing.T, open Opener) {
	tests := []struct {
		name string
		fn   func(*testing.T, limits.Store)
	}{
		{"AddAndAdjust", testAddAndAdjust},
		{"AddRace", testAddRace},
		{"AddAtTheLimit", testAddAtTheLimit},
		{"AdjustMovesToANewerWindow", testAdjustMovesToANewerWindow},
		{"Take", testTake},
		{"TakeRace", testTakeRace},
		{"SettleKeepsDebt", testSettleKeepsDebt},
		{"HugeAndNegativeAmounts", testHugeAndNegativeAmounts},
		{"Acquire", testAcquire},
		{"AcquireRace", testAcquireRace},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, open(t)) })
	}
}

var ctx = context.Background()

func testAddAndAdjust(t *testing.T, s limits.Store) {
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	if r, err := s.Add(ctx, "user:a", 60, 100, limits.Day, now); err != nil || !r.OK || r.Used != 60 {
		t.Fatalf("first add %+v, %v", r, err)
	}
	if r, _ := s.Add(ctx, "user:a", 50, 100, limits.Day, now); r.OK || r.Used != 60 || r.Limit != 100 || !r.Reset.Equal(limits.Day.Reset(now)) {
		t.Fatalf("add over the limit %+v", r)
	}
	if err := s.Adjust(ctx, "user:a", -40, limits.Day, now); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Add(ctx, "user:a", 80, 100, limits.Day, now); !r.OK || r.Used != 100 {
		t.Fatalf("add after giving back %+v", r)
	}
	tomorrow := now.Add(24 * time.Hour)
	if r, _ := s.Add(ctx, "user:a", 100, 100, limits.Day, tomorrow); !r.OK || r.Used != 100 {
		t.Fatalf("new day %+v", r)
	}
	if err := s.Adjust(ctx, "user:a", -100, limits.Day, now); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Add(ctx, "user:a", 1, 100, limits.Day, tomorrow); r.OK {
		t.Fatalf("settling yesterday changed today: %+v", r)
	}
	if u, _ := s.Used(ctx, "user:a", limits.Day, tomorrow); u != 100 {
		t.Fatalf("used %d", u)
	}
	if u, _ := s.Used(ctx, "user:a", limits.Month, now); u != 0 {
		t.Fatalf("the monthly counter shares the daily one: %d", u)
	}
	if u, _ := s.Used(ctx, "user:b", limits.Day, now); u != 0 {
		t.Fatalf("an unknown key has used %d", u)
	}
	if err := s.Adjust(ctx, "user:a", -1000, limits.Day, tomorrow); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Used(ctx, "user:a", limits.Day, tomorrow); u != 0 {
		t.Fatalf("a give-back took the counter below zero: %d", u)
	}
}

func testAddRace(t *testing.T, s limits.Store) {
	now := time.Now()
	var ok atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			r, err := s.Add(ctx, "k", 1, 10, limits.Month, now)
			if err != nil {
				t.Error(err)
			}
			if r.OK {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 10 {
		t.Fatalf("%d adds passed a limit of 10", ok.Load())
	}
}

func testAddAtTheLimit(t *testing.T, s limits.Store) {
	now := time.Now()
	if r, _ := s.Add(ctx, "k", 10, 10, limits.Day, now); !r.OK {
		t.Fatal(r)
	}
	if r, _ := s.Add(ctx, "k", 0, 10, limits.Day, now); r.OK {
		t.Fatalf("a zero amount passed a spent limit: %+v", r)
	}
	// A limit of zero, such as a budget of 0 USD, refuses everything.
	for _, n := range []int64{1, 0} {
		if r, err := s.Add(ctx, "zero", n, 0, limits.Month, now); err != nil || r.OK {
			t.Fatalf("add %d under a limit of 0: %+v, %v", n, r, err)
		}
	}
}

func testAdjustMovesToANewerWindow(t *testing.T, s limits.Store) {
	oct := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	nov := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	_ = s.Adjust(ctx, "k", 7, limits.Month, oct)
	_ = s.Adjust(ctx, "k", 5, limits.Month, nov)
	if u, _ := s.Used(ctx, "k", limits.Month, nov); u != 5 {
		t.Fatalf("used %d, want 5", u)
	}
}

func testTake(t *testing.T, s limits.Store) {
	r := limits.Rate{N: 60, Per: time.Minute}
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	for i := range 60 {
		b, err := s.Take(ctx, "k", 1, r, now)
		if err != nil || !b.OK {
			t.Fatalf("call %d refused: %+v, %v", i, b, err)
		}
		if i == 0 && b.Remaining != 59 {
			t.Fatalf("remaining %d", b.Remaining)
		}
	}
	b, _ := s.Take(ctx, "k", 1, r, now)
	if b.OK || b.RetryAfter != time.Second || b.Remaining != 0 || b.Reset != time.Minute {
		t.Fatalf("61st call %+v", b)
	}
	if b, _ := s.Take(ctx, "k", 1, r, now.Add(time.Second)); !b.OK {
		t.Fatalf("after a second %+v", b)
	}
	if err := s.Settle(ctx, "k", -30, r, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Take(ctx, "k", 30, r, now.Add(time.Second)); !b.OK {
		t.Fatalf("after giving 30 back %+v", b)
	}
	if b, _ := s.Take(ctx, "other", 61, r, now); b.OK {
		t.Fatalf("a cost over the whole rate passed %+v", b)
	}
	if err := s.Settle(ctx, "fresh", 5, r, now); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Take(ctx, "fresh", 1, r, now); !b.OK || b.Remaining != 54 {
		t.Fatalf("a settle on an unused rate did not count: %+v", b)
	}
}

func testTakeRace(t *testing.T, s limits.Store) {
	r := limits.Rate{N: 10, Per: time.Hour}
	now := time.Now()
	var ok atomic.Int64
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			b, err := s.Take(ctx, "k", 1, r, now)
			if err != nil {
				t.Error(err)
			}
			if b.OK {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 10 {
		t.Fatalf("%d takes passed a rate of 10", ok.Load())
	}
}

func testSettleKeepsDebt(t *testing.T, s limits.Store) {
	rate := limits.Rate{N: 1000, Per: time.Minute}
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	if b, _ := s.Take(ctx, "r", 1000, rate, now); !b.OK {
		t.Fatal(b)
	}
	if err := s.Settle(ctx, "r", 31000, rate, now); err != nil {
		t.Fatal(err)
	}
	b, _ := s.Take(ctx, "r", 1, rate, now)
	if b.OK || b.RetryAfter < 31*time.Minute {
		t.Fatalf("a call 32 times the rate cost %s of waiting: %+v", b.RetryAfter, b)
	}
}

func testHugeAndNegativeAmounts(t *testing.T, s limits.Store) {
	now := time.Now()
	if r, _ := s.Add(ctx, "k", 5, 10, limits.Day, now); !r.OK {
		t.Fatal(r)
	}
	if r, _ := s.Add(ctx, "k", 1<<62, 10, limits.Day, now); r.OK {
		t.Fatalf("huge add passed: %+v", r)
	}
	if r, _ := s.Add(ctx, "k", -1<<62, 10, limits.Day, now); r.OK || r.Used != 5 {
		t.Fatalf("negative add changed the counter: %+v", r)
	}
	rate := limits.Rate{N: 10, Per: time.Minute}
	if b, _ := s.Take(ctx, "r", 1<<62, rate, now); b.OK {
		t.Fatalf("huge take passed: %+v", b)
	}
	if b, _ := s.Take(ctx, "r", -1<<62, rate, now); b.OK && b.Remaining > 10 {
		t.Fatalf("negative take added room: %+v", b)
	}
	if err := s.Settle(ctx, "r", 1<<62, rate, now); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Take(ctx, "r", 1, rate, now); b.OK {
		t.Fatalf("a huge settle did not fill the bucket: %+v", b)
	}
	if err := s.Settle(ctx, "r", -1<<62, rate, now); err != nil {
		t.Fatal(err)
	}
}

func testAcquire(t *testing.T, s limits.Store) {
	ok1, rel1, err1 := s.Acquire(ctx, "k", 2)
	ok2, rel2, err2 := s.Acquire(ctx, "k", 2)
	ok3, rel3, err3 := s.Acquire(ctx, "k", 2)
	if !ok1 || !ok2 || ok3 || err1 != nil || err2 != nil || err3 != nil {
		t.Fatalf("acquire %v %v %v (%v %v %v)", ok1, ok2, ok3, err1, err2, err3)
	}
	rel3() // a refused slot's release is harmless
	rel1()
	rel1()
	if ok, _, _ := s.Acquire(ctx, "k", 2); !ok {
		t.Fatal("slot not freed")
	}
	if ok, _, _ := s.Acquire(ctx, "k", 2); ok {
		t.Fatal("a release called twice freed two slots")
	}
	rel2()
	if ok, _, _ := s.Acquire(ctx, "other", 1); !ok {
		t.Fatal("keys share slots")
	}
}

func testAcquireRace(t *testing.T, s limits.Store) {
	var ok atomic.Int64
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			got, _, err := s.Acquire(ctx, "k", 4)
			if err != nil {
				t.Error(err)
			}
			if got {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 4 {
		t.Fatalf("%d slots taken of 4", ok.Load())
	}
}
