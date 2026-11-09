package sqlite

import (
	"context"
	"encoding/json"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

var _ store.UsageStore = (*DB)(nil)

// RecordUsage stores one usage record.
func (db *DB) RecordUsage(ctx context.Context, r store.UsageRecord) error {
	teams, err := json.Marshal(nonNil(r.Teams))
	if err != nil {
		return err
	}
	_, err = db.w.ExecContext(ctx, `INSERT INTO usage
		(org_id, time, request_id, user_id, teams, key, project, route, provider, model, endpoint,
		 input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, cost_nusd,
		 estimated, status, latency_ms, fallbacks)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.OrgID, r.Time.UnixMilli(), r.RequestID, r.User, string(teams), r.Key, r.Project, r.Route,
		r.Provider, r.Model, r.Endpoint, r.InputTokens, r.OutputTokens, r.CacheReadTokens,
		r.CacheWriteTokens, r.ReasoningTokens, r.CostNUSD, r.Estimated, r.Status, r.Latency.Milliseconds(), r.Fallbacks)
	return err
}

// RecentUsage returns up to limit records for orgID, newest first.
func (db *DB) RecentUsage(ctx context.Context, orgID string, limit int) ([]store.UsageRecord, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT org_id, time, request_id, user_id, teams, key, project,
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

// UsageSince calls fn for each of orgID's records from since on, oldest first, with the fields
// the limiter needs to rebuild its counters.
func (db *DB) UsageSince(ctx context.Context, orgID string, since time.Time, fn func(store.UsageRecord) error) error {
	rows, err := db.sql.QueryContext(ctx, `SELECT time, user_id, teams, key, input_tokens, output_tokens,
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
