// Package limits counts usage against budgets, token allowances and request rates.
package limits

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Rate is N units per Per.
type Rate struct {
	N   int64
	Per time.Duration
}

var units = map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}

// ParseRate reads a rate written as N/s, N/m, N/h or N/d.
func ParseRate(s string) (Rate, error) {
	num, unit, ok := strings.Cut(s, "/")
	per, known := units[unit]
	n, err := strconv.ParseInt(num, 10, 64)
	if !ok || !known || err != nil || n <= 0 {
		return Rate{}, fmt.Errorf("rate %q must be a positive number per s, m, h or d, such as 60/m", s)
	}
	return Rate{N: n, Per: per}, nil
}

func (r Rate) String() string {
	for u, d := range units {
		if d == r.Per {
			return strconv.FormatInt(r.N, 10) + "/" + u
		}
	}
	return strconv.FormatInt(r.N, 10) + "/" + r.Per.String()
}

// maxDebt is the longest a settled overrun can hold a rate back.
const maxDebt = 365 * 24 * time.Hour

// interval is the time one unit takes to come back.
func (r Rate) interval() time.Duration { return r.Per / time.Duration(r.N) }

// Window is a UTC calendar period.
type Window int

// Windows.
const (
	Day Window = iota + 1
	Month
)

// Start returns the start of the window holding now.
func (w Window) Start(now time.Time) time.Time {
	u := now.UTC()
	if w == Month {
		return time.Date(u.Year(), u.Month(), 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// Reset returns when the window holding now ends.
func (w Window) Reset(now time.Time) time.Time {
	if w == Month {
		return w.Start(now).AddDate(0, 1, 0)
	}
	return w.Start(now).AddDate(0, 0, 1)
}

func (w Window) String() string {
	if w == Month {
		return "monthly"
	}
	return "daily"
}

// Result is the outcome of an Add.
type Result struct {
	OK          bool
	Used, Limit int64
	Reset       time.Time
}

// Bucket is the outcome of a Take on a rate.
type Bucket struct {
	OK         bool
	RetryAfter time.Duration
	Remaining  int64
	Reset      time.Duration
}

// ErrStore wraps failures of a shared store, so callers can apply on_store_error.
var ErrStore = errors.New("limits: store unavailable")

// Store keeps the counters. The memory store serves one process; SQL and Valkey stores that
// share counters across replicas can implement the same interface.
type Store interface {
	// Add adds n to key's counter for the window holding now, unless the total would pass limit.
	Add(ctx context.Context, key string, n, limit int64, w Window, now time.Time) (Result, error)
	// Adjust changes key's counter for the window holding at, with no limit check. It is how a
	// reservation settles; a window that has already ended is left alone.
	Adjust(ctx context.Context, key string, delta int64, w Window, at time.Time) error
	// Used returns key's counter for the window holding now.
	Used(ctx context.Context, key string, w Window, now time.Time) (int64, error)
	// Take spends cost from key's rate with GCRA, unless the rate has no room for it.
	Take(ctx context.Context, key string, cost int64, r Rate, now time.Time) (Bucket, error)
	// Settle spends delta more (or gives it back when negative) on key's rate, with no check.
	Settle(ctx context.Context, key string, delta int64, r Rate, now time.Time) error
	// Acquire takes one of limit concurrent slots for key. release frees it and may be called
	// more than once.
	Acquire(ctx context.Context, key string, limit int64) (ok bool, release func(), err error)
}

type counter struct {
	start time.Time
	n     int64
}

// MemoryStore keeps every counter in this process.
type MemoryStore struct {
	mu       sync.Mutex
	counters map[string]*counter
	tats     map[string]time.Time
	inflight map[string]int64
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{counters: map[string]*counter{}, tats: map[string]time.Time{}, inflight: map[string]int64{}}
}

func windowKey(key string, w Window) string { return key + "|" + w.String() }

// current returns the counter for the window holding now, starting it afresh when the stored
// one belongs to an earlier window.
func (s *MemoryStore) current(key string, w Window, now time.Time) *counter {
	k := windowKey(key, w)
	start := w.Start(now)
	c, ok := s.counters[k]
	if !ok || c.start.Before(start) {
		c = &counter{start: start}
		s.counters[k] = c
	}
	return c
}

// Add implements Store.
func (s *MemoryStore) Add(_ context.Context, key string, n, limit int64, w Window, now time.Time) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.current(key, w, now)
	r := Result{Used: c.n, Limit: limit, Reset: w.Reset(now)}
	// Written as a subtraction so a huge n cannot overflow past the check.
	if n < 0 || n > limit-c.n {
		return r, nil
	}
	c.n += n
	r.OK, r.Used = true, c.n
	return r, nil
}

// Adjust implements Store.
func (s *MemoryStore) Adjust(_ context.Context, key string, delta int64, w Window, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.counters[windowKey(key, w)]
	if !ok || c.start.Before(w.Start(at)) {
		c = s.current(key, w, at)
	}
	if !c.start.Equal(w.Start(at)) {
		return nil
	}
	c.n = max(c.n+delta, 0)
	return nil
}

// Used implements Store.
func (s *MemoryStore) Used(_ context.Context, key string, w Window, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.current(key, w, now).n, nil
}

// Take implements Store with GCRA: the bucket holds up to N units and refills one unit every
// Per/N. A cost larger than N can never pass.
func (s *MemoryStore) Take(_ context.Context, key string, cost int64, r Rate, now time.Time) (Bucket, error) {
	if cost > r.N {
		return Bucket{RetryAfter: r.Per}, nil
	}
	cost = max(cost, 0)
	s.mu.Lock()
	defer s.mu.Unlock()
	iv := r.interval()
	tat := s.tats[key]
	if tat.Before(now) {
		tat = now
	}
	next := tat.Add(time.Duration(cost) * iv)
	if allowAt := next.Add(-r.Per); allowAt.After(now) {
		b := Bucket{RetryAfter: allowAt.Sub(now), Reset: tat.Sub(now)}
		b.Remaining = remaining(r, tat.Sub(now))
		return b, nil
	}
	s.tats[key] = next
	return Bucket{OK: true, Remaining: remaining(r, next.Sub(now)), Reset: next.Sub(now)}, nil
}

func remaining(r Rate, ahead time.Duration) int64 {
	return max(int64((r.Per-ahead)/r.interval()), 0)
}

// Settle implements Store.
func (s *MemoryStore) Settle(_ context.Context, key string, delta int64, r Rate, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Debt carries over, so a call far over the rate waits as long as it used. It is capped at a
	// year of the rate, which keeps the duration math from overflowing. A give-back is at most
	// the N a reservation can hold.
	iv := r.interval()
	delta = min(max(delta, -r.N), int64(maxDebt/iv))
	tat := s.tats[key]
	if tat.Before(now) {
		tat = now
	}
	tat = tat.Add(time.Duration(delta) * iv)
	if tat.Before(now) {
		tat = now
	}
	s.tats[key] = tat
	return nil
}

// Acquire implements Store.
func (s *MemoryStore) Acquire(_ context.Context, key string, limit int64) (bool, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight[key] >= limit {
		return false, func() {}, nil
	}
	s.inflight[key]++
	var once sync.Once
	return true, func() {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.inflight[key]--
		})
	}, nil
}

// InFlight returns how many slots of key are taken.
func (s *MemoryStore) InFlight(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight[key]
}
