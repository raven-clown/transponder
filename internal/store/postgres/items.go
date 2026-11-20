package postgres

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/raven-clown/transponder/internal/store"
)

var _ store.ConfigItemStore = (*DB)(nil)

const itemColumns = `org_id, kind, id, position, body, secret, version, source, created_at, updated_at`

// ListConfigItems implements store.ConfigItemStore.
func (db *DB) ListConfigItems(ctx context.Context, orgID, kind string) ([]store.ConfigItem, error) {
	rows, err := db.q(ctx).Query(ctx, `SELECT `+itemColumns+` FROM config_items
		WHERE org_id = $1 AND kind = $2 ORDER BY position, id COLLATE "C"`, orgID, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
	return scanItem(db.q(ctx).QueryRow(ctx, `SELECT `+itemColumns+` FROM config_items
		WHERE org_id = $1 AND kind = $2 AND id = $3`, orgID, kind, id))
}

// CreateConfigItem implements store.ConfigItemStore.
func (db *DB) CreateConfigItem(ctx context.Context, it store.ConfigItem) error {
	updated := it.UpdatedAt
	if updated.IsZero() {
		updated = it.CreatedAt
	}
	_, err := db.q(ctx).Exec(ctx, `INSERT INTO config_items (`+itemColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, 1, $7, $8, $9)`,
		it.OrgID, it.Kind, it.ID, it.Position, string(it.Body), it.Secret, cmp.Or(it.Source, store.SourceAPI),
		it.CreatedAt.UnixMilli(), updated.UnixMilli())
	if isUnique(err) {
		return fmt.Errorf("%s %q: %w", it.Kind, it.ID, store.ErrConflict)
	}
	return err
}

// UpdateConfigItem implements store.ConfigItemStore.
func (db *DB) UpdateConfigItem(ctx context.Context, it store.ConfigItem, version int64) (store.ConfigItem, error) {
	tag, err := db.q(ctx).Exec(ctx, `UPDATE config_items SET position = $1, body = $2, secret = $3,
		source = $4, updated_at = $5, version = version + 1
		WHERE org_id = $6 AND kind = $7 AND id = $8 AND ($9::bigint = 0 OR version = $9::bigint)`,
		it.Position, string(it.Body), it.Secret, cmp.Or(it.Source, store.SourceAPI), it.UpdatedAt.UnixMilli(),
		it.OrgID, it.Kind, it.ID, version)
	if err := db.changed(ctx, tag, err, `SELECT 1 FROM config_items WHERE org_id = $1 AND kind = $2 AND id = $3`, it.OrgID, it.Kind, it.ID); err != nil {
		return store.ConfigItem{}, err
	}
	return db.ConfigItem(ctx, it.OrgID, it.Kind, it.ID)
}

// DeleteConfigItem implements store.ConfigItemStore.
func (db *DB) DeleteConfigItem(ctx context.Context, orgID, kind, id string, version int64) error {
	tag, err := db.q(ctx).Exec(ctx, `DELETE FROM config_items
		WHERE org_id = $1 AND kind = $2 AND id = $3 AND ($4::bigint = 0 OR version = $4::bigint)`, orgID, kind, id, version)
	return db.changed(ctx, tag, err, `SELECT 1 FROM config_items WHERE org_id = $1 AND kind = $2 AND id = $3`, orgID, kind, id)
}

func scanItem(row pgx.Row) (store.ConfigItem, error) {
	var (
		it               store.ConfigItem
		body             string
		created, updated int64
	)
	err := row.Scan(&it.OrgID, &it.Kind, &it.ID, &it.Position, &body, &it.Secret, &it.Version, &it.Source, &created, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
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
