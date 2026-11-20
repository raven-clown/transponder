package sqlite

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/raven-clown/transponder/internal/store"
)

var _ store.KeyAdminStore = (*DB)(nil)

const keyColumns = `org_id, name, hash, last4, role, teams, disabled, expires_at, created_at, version, source, updated_at`

// CreateKey stores a new API key.
func (db *DB) CreateKey(ctx context.Context, k store.APIKey) error {
	teams, err := json.Marshal(nonNil(k.Teams))
	if err != nil {
		return err
	}
	updated := k.UpdatedAt
	if updated.IsZero() {
		updated = k.CreatedAt
	}
	_, err = db.writer(ctx).ExecContext(ctx, `INSERT INTO api_keys
		(org_id, name, hash, last4, role, teams, disabled, expires_at, created_at, updated_at, version, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)`,
		k.OrgID, k.Name, k.Hash[:], k.Last4, k.Role, string(teams), k.Disabled,
		nullMillis(k.ExpiresAt), k.CreatedAt.UnixMilli(), updated.UnixMilli(), cmp.Or(k.Source, store.SourceAPI))
	if isConstraint(err) {
		return fmt.Errorf("key %q: %w", k.Name, store.ErrConflict)
	}
	return err
}

// KeyByHash returns the key whose secret hashes to hash.
func (db *DB) KeyByHash(ctx context.Context, hash [32]byte) (store.APIKey, error) {
	return scanKey(db.reader(ctx).QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE hash = ?`, hash[:]))
}

// KeyByName returns orgID's key called name.
func (db *DB) KeyByName(ctx context.Context, orgID, name string) (store.APIKey, error) {
	return scanKey(db.reader(ctx).QueryRowContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE org_id = ? AND name = ?`, orgID, name))
}

// ListKeys returns orgID's keys sorted by name.
func (db *DB) ListKeys(ctx context.Context, orgID string) ([]store.APIKey, error) {
	rows, err := db.reader(ctx).QueryContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE org_id = ? ORDER BY name`, orgID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []store.APIKey
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// UpdateKey implements store.KeyAdminStore.
func (db *DB) UpdateKey(ctx context.Context, k store.APIKey, version int64) (store.APIKey, error) {
	teams, err := json.Marshal(nonNil(k.Teams))
	if err != nil {
		return store.APIKey{}, err
	}
	res, err := db.writer(ctx).ExecContext(ctx, `UPDATE api_keys SET hash = ?, last4 = ?, role = ?, teams = ?,
		disabled = ?, expires_at = ?, updated_at = ?, version = version + 1
		WHERE org_id = ? AND name = ? AND (? = 0 OR version = ?)`,
		k.Hash[:], k.Last4, k.Role, string(teams), k.Disabled, nullMillis(k.ExpiresAt), k.UpdatedAt.UnixMilli(),
		k.OrgID, k.Name, version, version)
	if isConstraint(err) {
		return store.APIKey{}, fmt.Errorf("key %q: %w", k.Name, store.ErrConflict)
	}
	if err := db.changed(ctx, res, err, `SELECT 1 FROM api_keys WHERE org_id = ? AND name = ?`, k.OrgID, k.Name); err != nil {
		return store.APIKey{}, err
	}
	return db.KeyByName(ctx, k.OrgID, k.Name)
}

// DeleteKey implements store.KeyAdminStore.
func (db *DB) DeleteKey(ctx context.Context, orgID, name string, version int64) error {
	res, err := db.writer(ctx).ExecContext(ctx, `DELETE FROM api_keys WHERE org_id = ? AND name = ? AND (? = 0 OR version = ?)`,
		orgID, name, version, version)
	return db.changed(ctx, res, err, `SELECT 1 FROM api_keys WHERE org_id = ? AND name = ?`, orgID, name)
}

// changed turns a conditional write that touched no row into ErrNotFound when exists finds
// nothing, and ErrVersionConflict when the row is there with another version.
func (db *DB) changed(ctx context.Context, res sql.Result, err error, exists string, args ...any) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n > 0 {
		return err
	}
	var one int
	err = db.writer(ctx).QueryRowContext(ctx, exists, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	return store.ErrVersionConflict
}

type scanner interface {
	Scan(dest ...any) error
}

func scanKey(row scanner) (store.APIKey, error) {
	var (
		k                store.APIKey
		h                []byte
		teams            string
		expires          sql.NullInt64
		created, updated int64
		disabled         bool
	)
	err := row.Scan(&k.OrgID, &k.Name, &h, &k.Last4, &k.Role, &teams, &disabled, &expires, &created, &k.Version, &k.Source, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return store.APIKey{}, store.ErrNotFound
	}
	if err != nil {
		return store.APIKey{}, err
	}
	copy(k.Hash[:], h)
	k.Disabled = disabled
	if err := json.Unmarshal([]byte(teams), &k.Teams); err != nil {
		return store.APIKey{}, fmt.Errorf("key %q teams: %w", k.Name, err)
	}
	if len(k.Teams) == 0 {
		k.Teams = nil
	}
	if expires.Valid {
		k.ExpiresAt = time.UnixMilli(expires.Int64)
	}
	k.CreatedAt = time.UnixMilli(created)
	k.UpdatedAt = time.UnixMilli(updated)
	return k, nil
}

// HasAdminKey reports whether orgID has an enabled, unexpired admin key at now.
func (db *DB) HasAdminKey(ctx context.Context, orgID string, now time.Time) (bool, error) {
	var n int
	err := db.reader(ctx).QueryRowContext(ctx, `SELECT count(*) FROM api_keys
		WHERE org_id = ? AND role = 'admin' AND disabled = 0 AND (expires_at IS NULL OR expires_at > ?)`,
		orgID, now.UnixMilli()).Scan(&n)
	return n > 0, err
}

func isConstraint(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	return se.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY || se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

func nullMillis(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixMilli(), Valid: true}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
