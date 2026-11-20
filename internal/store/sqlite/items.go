package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

var _ store.ConfigItemStore = (*DB)(nil)

const itemColumns = `org_id, kind, id, position, body, secret, version, source, created_at, updated_at`

// ListConfigItems implements store.ConfigItemStore.
func (db *DB) ListConfigItems(ctx context.Context, orgID, kind string) ([]store.ConfigItem, error) {
	rows, err := db.reader(ctx).QueryContext(ctx, `SELECT `+itemColumns+` FROM config_items
		WHERE org_id = ? AND kind = ? ORDER BY position, id`, orgID, kind)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.ConfigItem
	for rows.Next() {
		it, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// ConfigItem implements store.ConfigItemStore.
func (db *DB) ConfigItem(ctx context.Context, orgID, kind, id string) (store.ConfigItem, error) {
	return scanItem(db.reader(ctx).QueryRowContext(ctx, `SELECT `+itemColumns+` FROM config_items
		WHERE org_id = ? AND kind = ? AND id = ?`, orgID, kind, id))
}

// CreateConfigItem implements store.ConfigItemStore.
func (db *DB) CreateConfigItem(ctx context.Context, it store.ConfigItem) error {
	updated := it.UpdatedAt
	if updated.IsZero() {
		updated = it.CreatedAt
	}
	_, err := db.writer(ctx).ExecContext(ctx, `INSERT INTO config_items (`+itemColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		it.OrgID, it.Kind, it.ID, it.Position, string(it.Body), it.Secret, cmp.Or(it.Source, store.SourceAPI),
		it.CreatedAt.UnixMilli(), updated.UnixMilli())
	if isConstraint(err) {
		return fmt.Errorf("%s %q: %w", it.Kind, it.ID, store.ErrConflict)
	}
	return err
}

// UpdateConfigItem implements store.ConfigItemStore.
func (db *DB) UpdateConfigItem(ctx context.Context, it store.ConfigItem, version int64) (store.ConfigItem, error) {
	res, err := db.writer(ctx).ExecContext(ctx, `UPDATE config_items SET position = ?, body = ?, secret = ?,
		source = ?, updated_at = ?, version = version + 1
		WHERE org_id = ? AND kind = ? AND id = ? AND (? = 0 OR version = ?)`,
		it.Position, string(it.Body), it.Secret, cmp.Or(it.Source, store.SourceAPI), it.UpdatedAt.UnixMilli(),
		it.OrgID, it.Kind, it.ID, version, version)
	if err := db.changed(ctx, res, err, `SELECT 1 FROM config_items WHERE org_id = ? AND kind = ? AND id = ?`, it.OrgID, it.Kind, it.ID); err != nil {
		return store.ConfigItem{}, err
	}
	return db.ConfigItem(ctx, it.OrgID, it.Kind, it.ID)
}

// DeleteConfigItem implements store.ConfigItemStore.
func (db *DB) DeleteConfigItem(ctx context.Context, orgID, kind, id string, version int64) error {
	res, err := db.writer(ctx).ExecContext(ctx, `DELETE FROM config_items
		WHERE org_id = ? AND kind = ? AND id = ? AND (? = 0 OR version = ?)`, orgID, kind, id, version, version)
	return db.changed(ctx, res, err, `SELECT 1 FROM config_items WHERE org_id = ? AND kind = ? AND id = ?`, orgID, kind, id)
}

func scanItem(row scanner) (store.ConfigItem, error) {
	var (
		it               store.ConfigItem
		body             string
		created, updated int64
	)
	err := row.Scan(&it.OrgID, &it.Kind, &it.ID, &it.Position, &body, &it.Secret, &it.Version, &it.Source, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ConfigItem{}, store.ErrNotFound
	}
	if err != nil {
		return store.ConfigItem{}, err
	}
	it.Body = []byte(body)
	it.CreatedAt = time.UnixMilli(created)
	it.UpdatedAt = time.UnixMilli(updated)
	return it, nil
}
