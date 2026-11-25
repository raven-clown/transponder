package postgres

import (
	"context"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

var _ store.AuditStore = (*DB)(nil)

// AddAudit stores one audit record.
func (db *DB) AddAudit(ctx context.Context, r store.AuditRecord) error {
	diff := r.Diff
	if len(diff) == 0 {
		diff = []byte("{}")
	}
	_, err := db.q(ctx).Exec(ctx, `INSERT INTO audit (org_id, time, actor, action, kind, resource, diff, request_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		r.OrgID, r.Time.UnixMilli(), r.Actor, r.Action, r.Kind, r.Resource, string(diff), r.RequestID)
	return err
}

// ListAudit returns the records q selects, newest first.
func (db *DB) ListAudit(ctx context.Context, q store.AuditQuery) ([]store.AuditRecord, error) {
	f := newFilter("org_id", q.OrgID)
	f.eq("actor", q.Actor)
	f.eq("action", q.Action)
	f.eq("kind", q.Kind)
	f.eq("resource", q.Resource)
	f.timeRange(q.From, q.To)
	f.before(q.BeforeID)
	rows, err := db.q(ctx).Query(ctx, `SELECT id, org_id, time, actor, action, kind, resource, diff::text, request_id
		FROM audit WHERE `+f.where()+` ORDER BY id DESC LIMIT `+f.arg(q.Limit), f.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.AuditRecord
	for rows.Next() {
		var (
			r    store.AuditRecord
			at   int64
			diff string
		)
		if err := rows.Scan(&r.ID, &r.OrgID, &at, &r.Actor, &r.Action, &r.Kind, &r.Resource, &diff, &r.RequestID); err != nil {
			return nil, err
		}
		r.Time = time.UnixMilli(at)
		r.Diff = []byte(diff)
		out = append(out, r)
	}
	return out, rows.Err()
}
