// Package runtime holds the live configuration and reloads it when the config_version row in the
// store changes, as design section 9 describes. A request takes the current snapshot once and
// keeps it to the end, so a reload never changes a call halfway.
package runtime

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultInterval is how often each replica checks config_version, so a change reaches every
// replica within it.
const DefaultInterval = 2 * time.Second

// Versions reads the config version from the store.
type Versions interface {
	ConfigVersion(ctx context.Context) (int64, error)
}

// Loader loads and validates the whole runtime config. It is called with the version that was
// read just before, so the snapshot can say which version it holds.
type Loader[T any] func(ctx context.Context, version int64) (T, error)

// Snapshot is one loaded config.
type Snapshot[T any] struct {
	Version  int64
	Value    T
	LoadedAt time.Time
}

// Status is the reload state, for /admin/v1/status.
type Status struct {
	// Version is the config version being served.
	Version  int64
	LoadedAt time.Time
	// Failed is set when the store holds a newer version that did not load; the old one keeps
	// serving.
	Failed        bool
	FailedVersion int64
	Error         string
	FailedAt      time.Time
}

// Reloader keeps the current snapshot and swaps in a new one when the version changes.
type Reloader[T any] struct {
	versions Versions
	load     Loader[T]
	log      *slog.Logger
	now      func() time.Time

	cur    atomic.Pointer[Snapshot[T]]
	mu     sync.Mutex // one load at a time
	status atomic.Pointer[Status]
}

// New loads the config at the store's current version. A config that does not load at start is
// an error, since there is nothing older to keep serving.
func New[T any](ctx context.Context, versions Versions, load Loader[T], logger *slog.Logger, now func() time.Time) (*Reloader[T], error) {
	r := &Reloader[T]{versions: versions, load: load, log: logger, now: now}
	v, err := versions.ConfigVersion(ctx)
	if err != nil {
		return nil, err
	}
	val, err := load(ctx, v)
	if err != nil {
		return nil, err
	}
	snap := &Snapshot[T]{Version: v, Value: val, LoadedAt: now()}
	r.cur.Store(snap)
	r.status.Store(&Status{Version: v, LoadedAt: snap.LoadedAt})
	return r, nil
}

// Current returns the snapshot being served. Take it once per request and keep it.
func (r *Reloader[T]) Current() *Snapshot[T] { return r.cur.Load() }

// Status returns the reload state.
func (r *Reloader[T]) Status() Status { return *r.status.Load() }

// Check reads the version and reloads when it differs from the one being served. It returns
// whether a new config was swapped in. A config that fails to load leaves the old one serving
// and is logged once per version and error.
//
// The admin API calls it after a write commits, so on a single replica the change is live
// before the response returns. Every replica also calls it from Run.
func (r *Reloader[T]) Check(ctx context.Context) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, err := r.versions.ConfigVersion(ctx)
	if err != nil {
		r.failed(ctx, r.Current().Version, err)
		return false, err
	}
	cur := r.Current()
	if v == cur.Version {
		r.recovered(cur)
		return false, nil
	}
	val, err := r.load(ctx, v)
	if err != nil {
		r.failed(ctx, v, err)
		return false, err
	}
	snap := &Snapshot[T]{Version: v, Value: val, LoadedAt: r.now()}
	r.cur.Store(snap)
	r.status.Store(&Status{Version: v, LoadedAt: snap.LoadedAt})
	r.log.InfoContext(ctx, "config reloaded", "from_version", cur.Version, "version", v)
	return true, nil
}

func (r *Reloader[T]) failed(ctx context.Context, version int64, err error) {
	old := r.status.Load()
	st := *old
	st.Failed, st.FailedVersion, st.Error, st.FailedAt = true, version, err.Error(), r.now()
	r.status.Store(&st)
	if !old.Failed || old.FailedVersion != version || old.Error != st.Error {
		r.log.ErrorContext(ctx, "config reload failed, the previous config keeps serving",
			"version", old.Version, "failed_version", version, "err", err)
	}
}

// recovered clears a failure once the store is readable again and still at the served version,
// such as after a bad version was replaced by a write that restored the old one.
func (r *Reloader[T]) recovered(cur *Snapshot[T]) {
	if r.status.Load().Failed {
		r.status.Store(&Status{Version: cur.Version, LoadedAt: cur.LoadedAt})
	}
}

// Run calls Check every interval until ctx ends.
func (r *Reloader[T]) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = r.Check(ctx)
		}
	}
}
