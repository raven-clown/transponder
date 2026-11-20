package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/raven-clown/transponder/internal/limits"
	"github.com/raven-clown/transponder/internal/store"
)

// slotLease is how long a concurrency slot outlives the replica that holds it, should that
// replica die without giving it back.
const slotLease = 30 * time.Second

// heartbeatEvery is how often a replica renews its slots and says it is alive.
var heartbeatEvery = 10 * time.Second

// Counters keeps the limit counters in Postgres, so a budget, a token allowance or a rate holds
// across every replica on the database. Each change is one atomic statement, so two replicas
// can never both take the last of an allowance.
type Counters struct {
	pool    *pgxpool.Pool
	org     string
	replica string
	log     *slog.Logger

	replicas atomic.Int64

	mu    sync.Mutex
	slots map[int64]bool // slots this replica holds

	stop context.CancelFunc
	done chan struct{}
}

var _ limits.Store = (*Counters)(nil)

// Counters returns the shared counter store and starts the heartbeat that keeps this replica's
// concurrency slots alive. Close stops it.
func (db *DB) Counters(ctx context.Context, logger *slog.Logger) (*Counters, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, err
	}
	hbCtx, stop := context.WithCancel(context.WithoutCancel(ctx))
	c := &Counters{
		pool:    db.pool,
		org:     store.DefaultOrg,
		replica: hex.EncodeToString(raw[:]),
		log:     logger,
		slots:   map[int64]bool{},
		stop:    stop,
		done:    make(chan struct{}),
	}
	c.replicas.Store(1)
	if err := c.heartbeat(ctx); err != nil {
		stop()
		return nil, fmt.Errorf("starting the shared limit counters: %w", err)
	}
	go c.beat(hbCtx)
	return c, nil
}

func (c *Counters) beat(ctx context.Context) {
	defer close(c.done)
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.heartbeat(ctx); err != nil && ctx.Err() == nil {
				c.log.WarnContext(ctx, "renewing concurrency slots", "err", err)
			}
		}
	}
}

// heartbeat marks this replica alive, renews the slots it holds, drops the ones it gave back
// while the database was out of reach, and counts the replicas alive.
func (c *Counters) heartbeat(ctx context.Context) error {
	c.mu.Lock()
	held := make([]int64, 0, len(c.slots))
	for id := range c.slots {
		held = append(held, id)
	}
	c.mu.Unlock()
	batch := &pgx.Batch{}
	batch.Queue(`INSERT INTO limit_replicas (id, seen_at) VALUES ($1, now())
		ON CONFLICT (id) DO UPDATE SET seen_at = excluded.seen_at`, c.replica)
	batch.Queue(`UPDATE limit_slots SET expires_at = now() + $2::bigint * interval '1 millisecond'
		WHERE replica = $1 AND id = ANY($3)`, c.replica, slotLease.Milliseconds(), held)
	batch.Queue(`DELETE FROM limit_slots WHERE replica = $1 AND NOT (id = ANY($2))`, c.replica, held)
	batch.Queue(`DELETE FROM limit_replicas WHERE seen_at < now() - $1::bigint * interval '1 millisecond'`, slotLease.Milliseconds())
	var n int64
	batch.Queue(`SELECT count(*) FROM limit_replicas`).QueryRow(func(row pgx.Row) error { return row.Scan(&n) })
	if err := c.pool.SendBatch(ctx, batch).Close(); err != nil {
		return err
	}
	c.replicas.Store(max(n, 1))
	return nil
}

// Replicas returns how many replicas were alive at the last heartbeat, at least one.
func (c *Counters) Replicas() int64 { return c.replicas.Load() }

// Close stops the heartbeat and gives back this replica's slots.
func (c *Counters) Close() error {
	c.stop()
	<-c.done
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.pool.Exec(ctx, `DELETE FROM limit_slots WHERE replica = $1`, c.replica)
	_, err2 := c.pool.Exec(ctx, `DELETE FROM limit_replicas WHERE id = $1`, c.replica)
	return errors.Join(err, err2)
}

func (c *Counters) fail(err error) error { return fmt.Errorf("%w: %w", limits.ErrStore, err) }

// Add implements limits.Store.
func (c *Counters) Add(ctx context.Context, key string, n, limit int64, w limits.Window, now time.Time) (limits.Result, error) {
	start := w.Start(now)
	r := limits.Result{Limit: limit, Reset: w.Reset(now)}
	if n >= 0 && n <= limit && limit > 0 {
		// Written as a subtraction so a huge n cannot overflow past the check.
		err := c.pool.QueryRow(ctx, `INSERT INTO limit_counters AS c (org_id, key, win, start, n)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (org_id, key, win, start) DO UPDATE SET n = c.n + excluded.n
			WHERE c.n < $6::bigint AND excluded.n <= $6::bigint - c.n
			RETURNING n`, c.org, key, w.String(), start.UnixNano(), n, limit).Scan(&r.Used)
		if err == nil {
			r.OK = true
			return r, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return r, c.fail(err)
		}
	}
	used, err := c.Used(ctx, key, w, now)
	r.Used = used
	return r, err
}

// Adjust implements limits.Store. Each window start has its own row, so settling a window that
// has ended changes nothing the limiter reads.
func (c *Counters) Adjust(ctx context.Context, key string, delta int64, w limits.Window, at time.Time) error {
	_, err := c.pool.Exec(ctx, `INSERT INTO limit_counters AS c (org_id, key, win, start, n)
		VALUES ($1, $2, $3, $4, greatest($5::bigint, 0))
		ON CONFLICT (org_id, key, win, start) DO UPDATE SET n = greatest(c.n + $5::bigint, 0)`,
		c.org, key, w.String(), w.Start(at).UnixNano(), delta)
	if err != nil {
		return c.fail(err)
	}
	return nil
}

// Used implements limits.Store.
func (c *Counters) Used(ctx context.Context, key string, w limits.Window, now time.Time) (int64, error) {
	var n int64
	err := c.pool.QueryRow(ctx, `SELECT n FROM limit_counters WHERE org_id = $1 AND key = $2 AND win = $3 AND start = $4`,
		c.org, key, w.String(), w.Start(now).UnixNano()).Scan(&n)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, c.fail(err)
	}
	return n, nil
}

// Take implements limits.Store with GCRA, as the memory store does, with the arrival time kept
// in the database. The update only happens when the rate has room, in one statement.
func (c *Counters) Take(ctx context.Context, key string, cost int64, r limits.Rate, now time.Time) (limits.Bucket, error) {
	if cost > r.N {
		return limits.Bucket{RetryAfter: r.Per}, nil
	}
	step := max(cost, 0) * int64(r.Interval())
	var tat int64
	err := c.pool.QueryRow(ctx, `INSERT INTO limit_rates AS r (org_id, key, tat) VALUES ($1, $2, $3::bigint + $4::bigint)
		ON CONFLICT (org_id, key) DO UPDATE SET tat = greatest(r.tat, $3::bigint) + $4::bigint
		WHERE greatest(r.tat, $3::bigint) + $4::bigint - $5::bigint <= $3::bigint
		RETURNING tat`, c.org, key, now.UnixNano(), step, int64(r.Per)).Scan(&tat)
	if err == nil {
		ahead := time.Duration(tat - now.UnixNano())
		return limits.Bucket{OK: true, Remaining: r.Remaining(ahead), Reset: ahead}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return limits.Bucket{}, c.fail(err)
	}
	if err := c.pool.QueryRow(ctx, `SELECT tat FROM limit_rates WHERE org_id = $1 AND key = $2`, c.org, key).Scan(&tat); err != nil {
		return limits.Bucket{}, c.fail(err)
	}
	ahead := max(time.Duration(tat-now.UnixNano()), 0)
	return limits.Bucket{RetryAfter: ahead + time.Duration(step) - r.Per, Reset: ahead, Remaining: r.Remaining(ahead)}, nil
}

// Settle implements limits.Store.
func (c *Counters) Settle(ctx context.Context, key string, delta int64, r limits.Rate, now time.Time) error {
	step := r.SettleUnits(delta) * int64(r.Interval())
	_, err := c.pool.Exec(ctx, `INSERT INTO limit_rates AS r (org_id, key, tat) VALUES ($1, $2, greatest($3::bigint + $4::bigint, $3::bigint))
		ON CONFLICT (org_id, key) DO UPDATE SET tat = greatest(greatest(r.tat, $3::bigint) + $4::bigint, $3::bigint)`,
		c.org, key, now.UnixNano(), step)
	if err != nil {
		return c.fail(err)
	}
	return nil
}

// Acquire implements limits.Store. The slots of one key are counted under a lock on that key,
// and a slot whose replica stopped renewing it has expired and no longer counts.
func (c *Counters) Acquire(ctx context.Context, key string, limit int64) (bool, func(), error) {
	noop := func() {}
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return false, noop, c.fail(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// The lock is its own statement: each later one takes a fresh snapshot, so the count sees
	// a slot another replica committed while this one waited.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || '|' || $2::text, 0))`, c.org, key); err != nil {
		return false, noop, c.fail(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM limit_slots WHERE org_id = $1 AND key = $2 AND expires_at < now()`, c.org, key); err != nil {
		return false, noop, c.fail(err)
	}
	var taken int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM limit_slots WHERE org_id = $1 AND key = $2`, c.org, key).Scan(&taken); err != nil {
		return false, noop, c.fail(err)
	}
	if taken >= limit {
		if err := tx.Commit(ctx); err != nil {
			return false, noop, c.fail(err)
		}
		return false, noop, nil
	}
	var id int64
	if err := tx.QueryRow(ctx, `INSERT INTO limit_slots (org_id, key, replica, expires_at)
		VALUES ($1, $2, $3, now() + $4::bigint * interval '1 millisecond') RETURNING id`,
		c.org, key, c.replica, slotLease.Milliseconds()).Scan(&id); err != nil {
		return false, noop, c.fail(err)
	}
	c.mu.Lock()
	c.slots[id] = true
	c.mu.Unlock()
	if err := tx.Commit(ctx); err != nil {
		c.forget(id)
		return false, noop, c.fail(err)
	}
	var once sync.Once
	return true, func() { once.Do(func() { c.release(id) }) }, nil
}

func (c *Counters) forget(id int64) {
	c.mu.Lock()
	delete(c.slots, id)
	c.mu.Unlock()
}

// release gives a slot back. When the database cannot be reached the slot is still forgotten
// here, so the next heartbeat deletes it.
func (c *Counters) release(id int64) {
	c.forget(id)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = c.pool.Exec(ctx, `DELETE FROM limit_slots WHERE id = $1`, id)
}

// SeedOnce fills the shared counters from the counters build returns, the first time any
// replica starts on this database with shared limits. After that every call is counted in the
// database as it happens, so filling them again would count it twice.
func (c *Counters) SeedOnce(ctx context.Context, build func() (*limits.MemoryStore, error)) (bool, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('limit_seeded|' || $1::text, 0))`, c.org); err != nil {
		return false, err
	}
	var seeded bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT FROM limit_seeded WHERE org_id = $1)`, c.org).Scan(&seeded); err != nil || seeded {
		return false, err
	}
	mem, err := build()
	if err != nil {
		return false, err
	}
	batch := &pgx.Batch{}
	mem.Each(func(key string, w limits.Window, start time.Time, n int64) {
		batch.Queue(`INSERT INTO limit_counters AS c (org_id, key, win, start, n) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (org_id, key, win, start) DO UPDATE SET n = c.n + excluded.n`,
			c.org, key, w.String(), start.UnixNano(), n)
	})
	batch.Queue(`INSERT INTO limit_seeded (org_id, seeded_at) VALUES ($1, $2)`, c.org, time.Now().UnixMilli())
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
