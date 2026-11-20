package limits

import (
	"context"
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

// A clock that jumps back across midnight, as when NTP corrects it, keeps counting on the later
// day rather than handing out the earlier day's allowance again. A forward jump starts the new
// window as the real midnight would.
func TestMemoryClockJumps(t *testing.T) {
	s := NewMemoryStore()
	after := time.Date(2026, 11, 21, 0, 0, 30, 0, time.UTC)
	before := after.Add(-time.Minute)
	if r, _ := s.Add(ctx, "k", 8, 10, Day, after); !r.OK {
		t.Fatal(r)
	}
	if r, _ := s.Add(ctx, "k", 5, 10, Day, before); r.OK || r.Used != 8 {
		t.Fatalf("after a jump back: %+v, want 8 of 10 used and the add refused", r)
	}
	if err := s.Adjust(ctx, "k", -8, Day, before); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.Used(ctx, "k", Day, after); u != 8 {
		t.Fatalf("a settlement dated before the jump changed the later day: used %d", u)
	}
	if r, _ := s.Add(ctx, "k", 10, 10, Day, after.Add(24*time.Hour)); !r.OK {
		t.Fatalf("the next day after a jump forward: %+v", r)
	}
}

func TestMemoryInFlight(t *testing.T) {
	s := NewMemoryStore()
	_, rel1, _ := s.Acquire(ctx, "k", 2)
	_, rel2, _ := s.Acquire(ctx, "k", 2)
	rel1()
	rel1()
	if n := s.InFlight("k"); n != 1 {
		t.Fatalf("in flight %d", n)
	}
	rel2()
}

func TestMemoryEach(t *testing.T) {
	s := NewMemoryStore()
	now := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	_, _ = s.Add(ctx, "a", 3, 10, Day, now)
	_, _ = s.Add(ctx, "a", 4, 10, Month, now)
	got := map[Window]int64{}
	s.Each(func(key string, w Window, start time.Time, n int64) {
		if key != "a" || !start.Equal(w.Start(now)) {
			t.Errorf("counter %s %s %v", key, w, start)
		}
		got[w] = n
	})
	if got[Day] != 3 || got[Month] != 4 || len(got) != 2 {
		t.Fatalf("counters %v", got)
	}
}
func TestParseRateTooFast(t *testing.T) {
	if _, err := ParseRate("2000000000/s"); err == nil {
		t.Fatal("a rate faster than one per nanosecond was accepted")
	}
}
