package limits

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// Failover counts in a shared store, and decides what happens while that store fails, as
// on_store_error says. With Deny unset each replica counts in Local with its share of every
// limit, so the service keeps running with stricter limits until the shared store answers.
type Failover struct {
	Shared Store
	Local  *MemoryStore
	// Deny refuses calls while the shared store fails, instead of counting locally.
	Deny bool
	// Replicas returns how many replicas share the limits, at least one.
	Replicas func() int64
	// OnChange is called when the shared store starts failing (err set) and when it answers
	// again (err nil), so the change is logged once and not on every call.
	OnChange func(err error)

	down atomic.Bool
}

var _ Store = (*Failover)(nil)

// check records whether the shared store answered and says whether to fall back.
func (f *Failover) check(err error) (fallback bool, out error) {
	if err == nil {
		if f.down.CompareAndSwap(true, false) && f.OnChange != nil {
			f.OnChange(nil)
		}
		return false, nil
	}
	if !f.down.Swap(true) && f.OnChange != nil {
		f.OnChange(err)
	}
	if f.Deny {
		return false, fmt.Errorf("%w: %w", ErrStore, err)
	}
	return true, nil
}

// share is this replica's part of limit, never less than one.
func (f *Failover) share(limit int64) int64 {
	n := int64(1)
	if f.Replicas != nil {
		n = max(f.Replicas(), 1)
	}
	return max(limit/n, 1)
}

func (f *Failover) shareRate(r Rate) Rate { return Rate{N: f.share(r.N), Per: r.Per} }

// Add implements Store.
func (f *Failover) Add(ctx context.Context, key string, n, limit int64, w Window, now time.Time) (Result, error) {
	r, err := f.Shared.Add(ctx, key, n, limit, w, now)
	if fallback, err := f.check(err); !fallback {
		return r, err
	}
	return f.Local.Add(ctx, key, n, f.share(limit), w, now)
}

// Adjust implements Store. A settlement during an outage lands in Local, so a reservation made
// in the shared store just before it is not given back there; the shared count stays high by
// that amount, which errs on the safe side.
func (f *Failover) Adjust(ctx context.Context, key string, delta int64, w Window, at time.Time) error {
	err := f.Shared.Adjust(ctx, key, delta, w, at)
	if fallback, err := f.check(err); !fallback {
		return err
	}
	return f.Local.Adjust(ctx, key, delta, w, at)
}

// Used implements Store.
func (f *Failover) Used(ctx context.Context, key string, w Window, now time.Time) (int64, error) {
	n, err := f.Shared.Used(ctx, key, w, now)
	if fallback, err := f.check(err); !fallback {
		return n, err
	}
	return f.Local.Used(ctx, key, w, now)
}

// Take implements Store.
func (f *Failover) Take(ctx context.Context, key string, cost int64, r Rate, now time.Time) (Bucket, error) {
	b, err := f.Shared.Take(ctx, key, cost, r, now)
	if fallback, err := f.check(err); !fallback {
		return b, err
	}
	return f.Local.Take(ctx, key, cost, f.shareRate(r), now)
}

// Settle implements Store.
func (f *Failover) Settle(ctx context.Context, key string, delta int64, r Rate, now time.Time) error {
	err := f.Shared.Settle(ctx, key, delta, r, now)
	if fallback, err := f.check(err); !fallback {
		return err
	}
	return f.Local.Settle(ctx, key, delta, f.shareRate(r), now)
}

// Acquire implements Store.
func (f *Failover) Acquire(ctx context.Context, key string, limit int64) (bool, func(), error) {
	ok, release, err := f.Shared.Acquire(ctx, key, limit)
	if fallback, err := f.check(err); !fallback {
		return ok, release, err
	}
	return f.Local.Acquire(ctx, key, f.share(limit))
}
