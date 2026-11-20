package postgres

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/raven-clown/transponder/internal/store"
)

var _ store.KeyAdminStore = (*DB)(nil)

// uniqueViolation is the SQLSTATE of a broken primary key or unique constraint.
const uniqueViolation = "23505"

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
	_, err = db.q(ctx).Exec(ctx, `INSERT INTO api_keys
		(org_id, name, hash, last4, role, teams, disabled, expires_at, created_at, updated_at, version, source)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 1, $11)`,
		k.OrgID, k.Name, k.Hash[:], k.Last4, k.Role, teams, k.Disabled,
		nullMillis(k.ExpiresAt), k.CreatedAt.UnixMilli(), updated.UnixMilli(), cmp.Or(k.Source, store.SourceAPI))
	if isUnique(err) {
		return fmt.Errorf("key %q: %w", k.Name, store.ErrConflict)
	}
	return err
}

// KeyByHash returns the key whose secret hashes to hash.
func (db *DB) KeyByHash(ctx context.Context, hash [32]byte) (store.APIKey, error) {
	return scanKey(db.q(ctx).QueryRow(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE hash = $1`, hash[:]))
}

// KeyByName returns orgID's key called name.
func (db *DB) KeyByName(ctx context.Context, orgID, name string) (store.APIKey, error) {
	return scanKey(db.q(ctx).QueryRow(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE org_id = $1 AND name = $2`, orgID, name))
}

// ListKeys returns orgID's keys sorted by name.
func (db *DB) ListKeys(ctx context.Context, orgID string) ([]store.APIKey, error) {
	rows, err := db.q(ctx).Query(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE org_id = $1 ORDER BY name COLLATE "C"`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
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
	tag, err := db.q(ctx).Exec(ctx, `UPDATE api_keys SET hash = $1, last4 = $2, role = $3, teams = $4,
		disabled = $5, expires_at = $6, updated_at = $7, version = version + 1
		WHERE org_id = $8 AND name = $9 AND ($10::bigint = 0 OR version = $10::bigint)`,
		k.Hash[:], k.Last4, k.Role, teams, k.Disabled, nullMillis(k.ExpiresAt), k.UpdatedAt.UnixMilli(),
		k.OrgID, k.Name, version)
	if isUnique(err) {
		return store.APIKey{}, fmt.Errorf("key %q: %w", k.Name, store.ErrConflict)
	}
	if err := db.changed(ctx, tag, err, `SELECT 1 FROM api_keys WHERE org_id = $1 AND name = $2`, k.OrgID, k.Name); err != nil {
		return store.APIKey{}, err
	}
	return db.KeyByName(ctx, k.OrgID, k.Name)
}

// DeleteKey implements store.KeyAdminStore.
func (db *DB) DeleteKey(ctx context.Context, orgID, name string, version int64) error {
	tag, err := db.q(ctx).Exec(ctx, `DELETE FROM api_keys WHERE org_id = $1 AND name = $2 AND ($3::bigint = 0 OR version = $3::bigint)`,
		orgID, name, version)
	return db.changed(ctx, tag, err, `SELECT 1 FROM api_keys WHERE org_id = $1 AND name = $2`, orgID, name)
}

// changed turns a conditional write that touched no row into ErrNotFound when exists finds
// nothing, and ErrVersionConflict when the row is there with another version.
func (db *DB) changed(ctx context.Context, tag pgconn.CommandTag, err error, exists string, args ...any) error {
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	var one int
	err = db.q(ctx).QueryRow(ctx, exists, args...).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}
	if err != nil {
		return err
	}
	return store.ErrVersionConflict
}

func scanKey(row pgx.Row) (store.APIKey, error) {
	var (
		k                store.APIKey
		h                []byte
		teams            []byte
		expires          *int64
		created, updated int64
	)
	err := row.Scan(&k.OrgID, &k.Name, &h, &k.Last4, &k.Role, &teams, &k.Disabled, &expires, &created, &k.Version, &k.Source, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return store.APIKey{}, store.ErrNotFound
	}
	if err != nil {
		return store.APIKey{}, err
	}
	copy(k.Hash[:], h)
	if err := json.Unmarshal(teams, &k.Teams); err != nil {
		return store.APIKey{}, fmt.Errorf("key %q teams: %w", k.Name, err)
	}
	if len(k.Teams) == 0 {
		k.Teams = nil
	}
	if expires != nil {
		k.ExpiresAt = time.UnixMilli(*expires)
	}
	k.CreatedAt = time.UnixMilli(created)
	k.UpdatedAt = time.UnixMilli(updated)
	return k, nil
}

// HasAdminKey reports whether orgID has an enabled, unexpired admin key at now.
func (db *DB) HasAdminKey(ctx context.Context, orgID string, now time.Time) (bool, error) {
	var has bool
	err := db.q(ctx).QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_keys
		WHERE org_id = $1 AND role = 'admin' AND NOT disabled AND (expires_at IS NULL OR expires_at > $2))`,
		orgID, now.UnixMilli()).Scan(&has)
	return has, err
}

func isUnique(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == uniqueViolation
}

func nullMillis(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	ms := t.UnixMilli()
	return &ms
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
