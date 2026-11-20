package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

var _ store.UsageAdminStore = (*DB)(nil)

const insertUsage = `INSERT INTO usage
	(org_id, time, request_id, user_id, teams, key, project, route, provider, model, endpoint,
	 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, cost_nusd,
	 estimated, status, latency_ms, fallbacks)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// maxUsageBatch caps the records one transaction writes, so the call that writes them does not
// wait for an unbounded queue.
const maxUsageBatch = 256

// usageBatch queues the usage records of calls waiting for SQLite's one writer, so they go in
// one transaction instead of one each. One waiting call at a time leads: it writes what is
// queued, then hands the lead to the first call still queued.
type usageBatch struct {
	mu      sync.Mutex
	pending []*usageWrite
	leading bool
}

type usageWrite struct {
	args []any
	// done gets the write's result, or lead when this call is to write the queue.
	done chan usageDone
}

type usageDone struct {
	err  error
	lead bool
}

func usageArgs(r store.UsageRecord) ([]any, error) {
	teams, err := json.Marshal(nonNil(r.Teams))
	if err != nil {
		return nil, err
	}
	return []any{r.OrgID, r.Time.UnixMilli(), r.RequestID, r.User, string(teams), r.Key, r.Project, r.Route,
		r.Provider, r.Model, r.Endpoint, r.InputTokens, r.OutputTokens, r.CacheReadTokens,
		r.CacheWriteTokens, r.ReasoningTokens, r.CostNUSD, r.Estimated, r.Status, r.Latency.Milliseconds(), r.Fallbacks}, nil
}

// RecordUsage stores one usage record. It returns once the record is committed, in a
// transaction it may share with other calls'.
func (db *DB) RecordUsage(ctx context.Context, r store.UsageRecord) error {
	args, err := usageArgs(r)
	if err != nil {
		return err
	}
	if tx, ok := ctx.Value(txKey{db}).(*sql.Tx); ok {
		_, err := tx.ExecContext(ctx, insertUsage, args...)
		return err
	}
	w := &usageWrite{args: args, done: make(chan usageDone, 1)}
	b := &db.usage
	b.mu.Lock()
	b.pending = append(b.pending, w)
	lead := !b.leading
	b.leading = true
	b.mu.Unlock()
	if !lead {
		if d := <-w.done; !d.lead {
			return d.err
		}
	}
	b.mu.Lock()
	n := min(len(b.pending), maxUsageBatch)
	batch := b.pending[:n:n]
	b.pending = b.pending[n:]
	b.mu.Unlock()
	db.writeUsage(context.WithoutCancel(ctx), batch)
	b.mu.Lock()
	if len(b.pending) > 0 {
		b.pending[0].done <- usageDone{lead: true}
	} else {
		b.leading = false
	}
	b.mu.Unlock()
	return (<-w.done).err
}

// writeUsage inserts batch in one transaction and tells each call how it went.
func (db *DB) writeUsage(ctx context.Context, batch []*usageWrite) {
	db.usageBatches.Add(1)
	err := func() error {
		if len(batch) == 1 {
			// Alone, the insert is its own transaction; BEGIN and COMMIT would only add round trips.
			_, err := db.insertUsage.ExecContext(ctx, batch[0].args...)
			return err
		}
		tx, err := db.w.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		stmt := tx.StmtContext(ctx, db.insertUsage)
		for _, w := range batch {
			if _, err := stmt.ExecContext(ctx, w.args...); err != nil {
				return err
			}
		}
		return tx.Commit()
	}()
	for _, w := range batch {
		w.done <- usageDone{err: err}
	}
}

// RecentUsage returns up to limit records for orgID, newest first.
func (db *DB) RecentUsage(ctx context.Context, orgID string, limit int) ([]store.UsageRecord, error) {
	rows, err := db.reader(ctx).QueryContext(ctx, `SELECT org_id, time, request_id, user_id, teams, key, project,
		route, provider, model, endpoint, input_tokens, output_tokens, cache_read_tokens,
		cache_write_tokens, reasoning_tokens, cost_nusd, estimated, status, latency_ms, fallbacks
		FROM usage WHERE org_id = ? ORDER BY time DESC, id DESC LIMIT ?`, orgID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.UsageRecord
	for rows.Next() {
		var (
			r         store.UsageRecord
			at, ms    int64
			teams     string
			estimated bool
		)
		if err := rows.Scan(&r.OrgID, &at, &r.RequestID, &r.User, &teams, &r.Key, &r.Project,
			&r.Route, &r.Provider, &r.Model, &r.Endpoint, &r.InputTokens, &r.OutputTokens,
			&r.CacheReadTokens, &r.CacheWriteTokens, &r.ReasoningTokens, &r.CostNUSD, &estimated, &r.Status, &ms,
			&r.Fallbacks); err != nil {
			return nil, err
		}
		r.Time = time.UnixMilli(at)
		r.Latency = time.Duration(ms) * time.Millisecond
		r.Estimated = estimated
		if err := json.Unmarshal([]byte(teams), &r.Teams); err != nil {
			return nil, err
		}
		if len(r.Teams) == 0 {
			r.Teams = nil
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListUsage implements store.UsageAdminStore.
func (db *DB) ListUsage(ctx context.Context, q store.UsageQuery) ([]store.UsageRecord, error) {
	where := []string{"org_id = ?"}
	args := []any{q.OrgID}
	add := func(cond string, v any) {
		where = append(where, cond)
		args = append(args, v)
	}
	if q.User != "" {
		add("user_id = ?", q.User)
	}
	if q.Key != "" {
		add("key = ?", q.Key)
	}
	if q.Project != "" {
		add("project = ?", q.Project)
	}
	if !q.From.IsZero() {
		add("time >= ?", q.From.UnixMilli())
	}
	if !q.To.IsZero() {
		add("time < ?", q.To.UnixMilli())
	}
	if q.BeforeID > 0 {
		add("id < ?", q.BeforeID)
	}
	args = append(args, q.Limit)
	rows, err := db.reader(ctx).QueryContext(ctx, `SELECT id, org_id, time, request_id, user_id, teams, key, project,
		route, provider, model, endpoint, input_tokens, output_tokens, cache_read_tokens,
		cache_write_tokens, reasoning_tokens, cost_nusd, estimated, status, latency_ms, fallbacks
		FROM usage WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.UsageRecord
	for rows.Next() {
		var (
			r         store.UsageRecord
			at, ms    int64
			teams     string
			estimated bool
		)
		if err := rows.Scan(&r.ID, &r.OrgID, &at, &r.RequestID, &r.User, &teams, &r.Key, &r.Project,
			&r.Route, &r.Provider, &r.Model, &r.Endpoint, &r.InputTokens, &r.OutputTokens,
			&r.CacheReadTokens, &r.CacheWriteTokens, &r.ReasoningTokens, &r.CostNUSD, &estimated, &r.Status, &ms,
			&r.Fallbacks); err != nil {
			return nil, err
		}
		r.Time = time.UnixMilli(at)
		r.Latency = time.Duration(ms) * time.Millisecond
		r.Estimated = estimated
		if err := json.Unmarshal([]byte(teams), &r.Teams); err != nil {
			return nil, err
		}
		if len(r.Teams) == 0 {
			r.Teams = nil
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageSince calls fn for each of orgID's records from since on, oldest first, with the fields
// the limiter needs to rebuild its counters.
func (db *DB) UsageSince(ctx context.Context, orgID string, since time.Time, fn func(store.UsageRecord) error) error {
	rows, err := db.reader(ctx).QueryContext(ctx, `SELECT time, user_id, teams, key, input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, reasoning_tokens, cost_nusd
		FROM usage WHERE org_id = ? AND time >= ? ORDER BY time, id`, orgID, since.UnixMilli())
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		r := store.UsageRecord{OrgID: orgID}
		var at int64
		var teams string
		if err := rows.Scan(&at, &r.User, &teams, &r.Key, &r.InputTokens, &r.OutputTokens,
			&r.CacheReadTokens, &r.CacheWriteTokens, &r.ReasoningTokens, &r.CostNUSD); err != nil {
			return err
		}
		r.Time = time.UnixMilli(at).UTC()
		if err := json.Unmarshal([]byte(teams), &r.Teams); err != nil {
			return err
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}
