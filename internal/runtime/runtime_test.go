package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// versions is a config_version row the test moves, and can make unreadable.
type versions struct {
	v    atomic.Int64
	fail atomic.Bool
}

func (s *versions) ConfigVersion(context.Context) (int64, error) {
	if s.fail.Load() {
		return 0, errors.New("store down")
	}
	return s.v.Load(), nil
}

// syncBuffer collects log lines from more than one goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

var t0 = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

// setup returns a reloader whose config at version v is "config v", except for the versions in
// bad, which fail to load.
func setup(t *testing.T, bad ...int64) (*Reloader[string], *versions, *syncBuffer) {
	t.Helper()
	vs := &versions{}
	vs.v.Store(1)
	logs := &syncBuffer{}
	load := func(_ context.Context, v int64) (string, error) {
		for _, b := range bad {
			if v == b {
				return "", fmt.Errorf("route %q uses provider %q, which is not declared", "gpt-*", "gone")
			}
		}
		return fmt.Sprintf("config %d", v), nil
	}
	r, err := New(context.Background(), vs, load, slog.New(slog.NewTextHandler(logs, nil)), func() time.Time { return t0 })
	if err != nil {
		t.Fatal(err)
	}
	return r, vs, logs
}

func TestNew(t *testing.T) {
	r, _, _ := setup(t)
	if s := r.Current(); s.Version != 1 || s.Value != "config 1" || !s.LoadedAt.Equal(t0) {
		t.Fatalf("snapshot %+v", s)
	}
	if st := r.Status(); st.Version != 1 || st.Failed {
		t.Fatalf("status %+v", st)
	}

	vs := &versions{}
	vs.fail.Store(true)
	ok := func(context.Context, int64) (string, error) { return "", nil }
	if _, err := New(context.Background(), vs, ok, slog.Default(), time.Now); err == nil {
		t.Fatal("New with an unreadable version succeeded")
	}
	vs.fail.Store(false)
	bad := func(context.Context, int64) (string, error) { return "", errors.New("bad") }
	if _, err := New(context.Background(), vs, bad, slog.Default(), time.Now); err == nil {
		t.Fatal("New with a config that does not load succeeded")
	}
}

func TestCheck(t *testing.T) {
	r, vs, logs := setup(t)
	ctx := context.Background()
	if changed, err := r.Check(ctx); changed || err != nil {
		t.Fatalf("Check with no change = %v, %v", changed, err)
	}
	// A request that started on version 1 keeps it after a reload.
	inFlight := r.Current()
	vs.v.Store(4)
	if changed, err := r.Check(ctx); !changed || err != nil {
		t.Fatalf("Check after a change = %v, %v", changed, err)
	}
	if s := r.Current(); s.Version != 4 || s.Value != "config 4" {
		t.Fatalf("snapshot %+v", s)
	}
	if inFlight.Version != 1 || inFlight.Value != "config 1" {
		t.Fatalf("the snapshot a request held changed: %+v", inFlight)
	}
	if !strings.Contains(logs.String(), "config reloaded") {
		t.Fatalf("logs: %s", logs)
	}
}

func TestCheckKeepsServingOnABadConfig(t *testing.T) {
	r, vs, logs := setup(t, 2)
	ctx := context.Background()
	vs.v.Store(2)
	for range 3 {
		if changed, err := r.Check(ctx); changed || err == nil {
			t.Fatalf("Check on a bad config = %v, %v", changed, err)
		}
	}
	if s := r.Current(); s.Version != 1 || s.Value != "config 1" {
		t.Fatalf("the old config stopped serving: %+v", s)
	}
	st := r.Status()
	if !st.Failed || st.Version != 1 || st.FailedVersion != 2 || !strings.Contains(st.Error, "not declared") || !st.FailedAt.Equal(t0) {
		t.Fatalf("status %+v", st)
	}
	if n := strings.Count(logs.String(), "config reload failed"); n != 1 {
		t.Fatalf("logged the same failure %d times: %s", n, logs)
	}

	// A later good version replaces it and clears the failure.
	vs.v.Store(3)
	if changed, err := r.Check(ctx); !changed || err != nil {
		t.Fatalf("Check after a fix = %v, %v", changed, err)
	}
	if st := r.Status(); st.Failed || st.Version != 3 {
		t.Fatalf("status after a fix %+v", st)
	}
}

func TestCheckWhenTheStoreIsDown(t *testing.T) {
	r, vs, logs := setup(t)
	ctx := context.Background()
	vs.fail.Store(true)
	for range 2 {
		if _, err := r.Check(ctx); err == nil {
			t.Fatal("Check with the store down succeeded")
		}
	}
	if st := r.Status(); !st.Failed || st.FailedVersion != 1 || r.Current().Version != 1 {
		t.Fatalf("status %+v", st)
	}
	if n := strings.Count(logs.String(), "config reload failed"); n != 1 {
		t.Fatalf("logged %d times: %s", n, logs)
	}
	vs.fail.Store(false)
	if _, err := r.Check(ctx); err != nil {
		t.Fatal(err)
	}
	if st := r.Status(); st.Failed {
		t.Fatalf("the failure stayed after the store came back: %+v", st)
	}
}

func TestRun(t *testing.T) {
	r, vs, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		r.Run(ctx, 5*time.Millisecond)
		close(done)
	}()
	vs.v.Store(2)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	deadline := time.After(5 * time.Second)
	for r.Current().Version != 2 {
		select {
		case <-tick.C:
		case <-deadline:
			t.Fatal("the loop did not pick up the new version")
		}
	}
	cancel()
	<-done
}

// Requests read the snapshot while reloads swap it.
func TestCurrentDuringReloads(t *testing.T) {
	r, vs, _ := setup(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := r.Current()
				if s.Value != fmt.Sprintf("config %d", s.Version) {
					t.Errorf("torn snapshot %+v", s)
					return
				}
			}
		})
	}
	for v := int64(2); v <= 50; v++ {
		vs.v.Store(v)
		if _, err := r.Check(ctx); err != nil {
			t.Fatal(err)
		}
	}
	close(stop)
	wg.Wait()
}
