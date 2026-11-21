package server_test

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/server"
)

// testClock is a clock a test moves by hand, so a wait measured against it is exact. A live
// one runs with the real clock plus what the test advanced it by, for a wait that something
// outside the test's reach measured on the real clock.
type testClock struct {
	mu   sync.Mutex
	t    time.Time
	live bool
	off  time.Duration
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.live {
		return time.Now().Add(c.off)
	}
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	c.off += d
}

func TestRateLimitRetryAfterCountsDown(t *testing.T) {
	// A Retry-After is a deadline, not a delay to restart after the last attempt: the time
	// the gateway spends trying the other targets comes off the wait it tells the caller.
	for name, tc := range map[string]struct {
		first   func(*testClock) string
		advance time.Duration
		want    int
		// live runs the gateway's clock with the real one: the provider turns a wait in
		// seconds into a deadline on the real clock, so a frozen clock would count the time
		// the test itself took as part of the wait.
		live bool
	}{
		"seconds": {
			first:   func(*testClock) string { return "30" },
			advance: 10 * time.Second,
			want:    20,
			live:    true,
		},
		"http date": {
			first: func(c *testClock) string {
				return c.Now().Add(time.Minute).UTC().Format(http.TimeFormat)
			},
			advance: 20 * time.Second,
			want:    40,
		},
	} {
		t.Run(name, func(t *testing.T) {
			// The gateway reads the clock a test can move, but the provider parses the header
			// with the real one. Starting the fake clock on a whole second keeps it just
			// behind the real clock, so a date-shaped wait is measured on the second it named
			// rather than a second late.
			clock := &testClock{t: time.Now().Truncate(time.Second), live: tc.live}
			a := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Retry-After", tc.first(clock))
				w.WriteHeader(http.StatusTooManyRequests)
			})
			b := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				clock.Advance(tc.advance)
				w.WriteHeader(http.StatusTooManyRequests)
			})
			e := setupDeps(t, route("*", oaTarget("a", "", a), oaTarget("b", "", b)), func(d *server.Deps) {
				d.Now = clock.Now
			})
			res := e.post(t, ping)
			if res.status != http.StatusTooManyRequests {
				t.Fatalf("status %d: %s", res.status, res.body)
			}
			// The provider reads the header with the real clock while the gateway reads the one
			// the test moves, and a date is spelled to the second, so the answer may come back
			// a second either side of the one named.
			got, err := strconv.Atoi(res.header.Get("Retry-After"))
			if err != nil || got < tc.want-1 || got > tc.want+1 {
				t.Fatalf("Retry-After %q, want %d±1s: the wait the first target asked for counts down while the second is tried", res.header.Get("Retry-After"), tc.want)
			}
		})
	}
}

func TestRateLimitKeepsAnExpiredDeadline(t *testing.T) {
	// A provider whose wait ran out before the answer was written is ready now. The caller
	// is told the earliest deadline, so a later provider's longer wait must not replace it;
	// without the expired deadline the answer would say 30 seconds instead of 1.
	a := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusTooManyRequests)
	})
	b := countedUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	e := setupDeps(t, route("*", oaTarget("a", "", a), oaTarget("b", "", b)), func(*server.Deps) {})
	res := e.post(t, ping)
	if res.status != http.StatusTooManyRequests {
		t.Fatalf("status %d: %s", res.status, res.body)
	}
	if got := res.header.Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After %q, want 1: the expired deadline is the earliest one given", got)
	}
}
