package sqlite

import (
	"context"
	"strings"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

var _ store.AuditStore = (*DB)(nil)

// AddAudit stores one audit record.
func (db *DB) AddAudit(ctx context.Context, r store.AuditRecord) error {
	diff := string(r.Diff)
	if diff == "" {
		diff = "{}"
	}
	_, err := db.writer(ctx).ExecContext(ctx, `INSERT INTO audit (org_id, time, actor, action, kind, resource, diff, request_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.OrgID, r.Time.UnixMilli(), r.Actor, r.Action, r.Kind, r.Resource, diff, r.RequestID)
	return err
}

// ListAudit returns the records q selects, newest first.
func (db *DB) ListAudit(ctx context.Context, q store.AuditQuery) ([]store.AuditRecord, error) {
	where := []string{"org_id = ?"}
	args := []any{q.OrgID}
	add := func(cond string, v any) {
		where = append(where, cond)
		args = append(args, v)
	}
	if q.Actor != "" {
		add("actor = ?", q.Actor)
	}
	if q.Action != "" {
		add("action = ?", q.Action)
	}
	if q.Kind != "" {
		add("kind = ?", q.Kind)
	}
	if q.Resource != "" {
		add("resource = ?", q.Resource)
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
	rows, err := db.reader(ctx).QueryContext(ctx, `SELECT id, org_id, time, actor, action, kind, resource, diff, request_id
		FROM audit WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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
