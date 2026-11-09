package limits

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var ctx = context.Background()

func TestParseRate(t *testing.T) {
	for in, want := range map[string]Rate{
		"60/m":    {N: 60, Per: time.Minute},
		"5/s":     {N: 5, Per: time.Second},
		"100/h":   {N: 100, Per: time.Hour},
		"20000/d": {N: 20000, Per: 24 * time.Hour},
	} {
		got, err := ParseRate(in)
		if err != nil || got != want {
			t.Errorf("ParseRate(%q) = %+v, %v", in, got, err)
		}
		if got.String() != in {
			t.Errorf("String() = %q, want %q", got.String(), in)
		}
	}
	for _, bad := range []string{"", "60", "0/m", "-1/m", "x/m", "60/w", "60/"} {
		if _, err := ParseRate(bad); err == nil {
			t.Errorf("ParseRate(%q) accepted", bad)
		}
	}
}

func TestWindows(t *testing.T) {
	now := time.Date(2026, 12, 31, 23, 30, 0, 0, time.FixedZone("ICT", 7*3600))
	utc := now.UTC()
	if got := Day.Start(now); !got.Equal(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)) || got.Location() != time.UTC {
		t.Fatalf("day start %v", got)
	}
	if got := Day.Reset(now); !got.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("day reset %v (now %v)", got, utc)
	}
	if got := Month.Start(now); !got.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("month start %v", got)
	}
	if got := Month.Reset(now); !got.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("month reset %v", got)
	}
}

func TestAddAndAdjust(t *testing.T) {
	s := NewMemoryStore()
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	if r, _ := s.Add(ctx, "user:a", 60, 100, Day, now); !r.OK || r.Used != 60 {
		t.Fatalf("first add %+v", r)
	}
	if r, _ := s.Add(ctx, "user:a", 50, 100, Day, now); r.OK || r.Used != 60 || r.Limit != 100 || !r.Reset.Equal(Day.Reset(now)) {
		t.Fatalf("add over the limit %+v", r)
	}
	if err := s.Adjust(ctx, "user:a", -40, Day, now); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Add(ctx, "user:a", 80, 100, Day, now); !r.OK || r.Used != 100 {
		t.Fatalf("add after giving back %+v", r)
	}
	tomorrow := now.Add(24 * time.Hour)
	if r, _ := s.Add(ctx, "user:a", 100, 100, Day, tomorrow); !r.OK || r.Used != 100 {
		t.Fatalf("new day %+v", r)
	}
	if err := s.Adjust(ctx, "user:a", -100, Day, now); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.Add(ctx, "user:a", 1, 100, Day, tomorrow); r.OK {
		t.Fatalf("settling yesterday changed today: %+v", r)
	}
	if u, _ := s.Used(ctx, "user:a", Day, tomorrow); u != 100 {
		t.Fatalf("used %d", u)
	}
}

func TestAddRace(t *testing.T) {
	s := NewMemoryStore()
	now := time.Now()
	var ok atomic.Int64
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			if r, _ := s.Add(ctx, "k", 1, 10, Month, now); r.OK {
				ok.Add(1)
			}
		})
	}
	wg.Wait()
	if ok.Load() != 10 {
		t.Fatalf("%d adds passed a limit of 10", ok.Load())
	}
}

func TestTake(t *testing.T) {
	s := NewMemoryStore()
	r := Rate{N: 60, Per: time.Minute}
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	for i := range 60 {
		b, _ := s.Take(ctx, "k", 1, r, now)
		if !b.OK {
			t.Fatalf("call %d refused: %+v", i, b)
		}
		if i == 0 && b.Remaining != 59 {
			t.Fatalf("remaining %d", b.Remaining)
		}
	}
	b, _ := s.Take(ctx, "k", 1, r, now)
	if b.OK || b.RetryAfter != time.Second || b.Remaining != 0 {
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
}

func TestAcquire(t *testing.T) {
	s := NewMemoryStore()
	ok1, rel1, _ := s.Acquire(ctx, "k", 2)
	ok2, rel2, _ := s.Acquire(ctx, "k", 2)
	ok3, _, _ := s.Acquire(ctx, "k", 2)
	if !ok1 || !ok2 || ok3 {
		t.Fatalf("acquire %v %v %v", ok1, ok2, ok3)
	}
	rel1()
	rel1()
	if ok, _, _ := s.Acquire(ctx, "k", 2); !ok {
		t.Fatal("slot not freed")
	}
	rel2()
	if n := s.InFlight("k"); n != 1 {
		t.Fatalf("in flight %d", n)
	}
}

func TestHugeAndNegativeAmounts(t *testing.T) {
	s := NewMemoryStore()
	now := time.Now()
	if r, _ := s.Add(ctx, "k", 5, 10, Day, now); !r.OK {
		t.Fatal(r)
	}
	if r, _ := s.Add(ctx, "k", 1<<62, 10, Day, now); r.OK {
		t.Fatalf("huge add passed: %+v", r)
	}
	if r, _ := s.Add(ctx, "k", -1<<62, 10, Day, now); r.OK || r.Used != 5 {
		t.Fatalf("negative add changed the counter: %+v", r)
	}
	rate := Rate{N: 10, Per: time.Minute}
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
	if b, _ := s.Take(ctx, "r", 10, rate, now); !b.OK {
		t.Fatalf("bucket did not empty after a huge give-back: %+v", b)
	}
}
