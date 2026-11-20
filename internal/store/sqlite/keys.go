package sqlite

import (
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

var _ store.KeyStore = (*DB)(nil)

// CreateKey stores a new API key.
func (db *DB) CreateKey(ctx context.Context, k store.APIKey) error {
	teams, err := json.Marshal(nonNil(k.Teams))
	if err != nil {
		return err
	}
	_, err = db.w.ExecContext(ctx, `INSERT INTO api_keys
		(org_id, name, hash, last4, role, teams, disabled, expires_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		k.OrgID, k.Name, k.Hash[:], k.Last4, k.Role, string(teams), k.Disabled,
		nullMillis(k.ExpiresAt), k.CreatedAt.UnixMilli(), k.CreatedAt.UnixMilli())
	if isConstraint(err) {
		return fmt.Errorf("key %q: %w", k.Name, store.ErrConflict)
	}
	return err
}

// KeyByHash returns the key whose secret hashes to hash.
func (db *DB) KeyByHash(ctx context.Context, hash [32]byte) (store.APIKey, error) {
	var (
		k        store.APIKey
		h        []byte
		teams    string
		expires  sql.NullInt64
		created  int64
		disabled bool
	)
	err := db.sql.QueryRowContext(ctx, `SELECT org_id, name, hash, last4, role, teams, disabled, expires_at, created_at
		FROM api_keys WHERE hash = ?`, hash[:]).
		Scan(&k.OrgID, &k.Name, &h, &k.Last4, &k.Role, &teams, &disabled, &expires, &created)
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
	return k, nil
}

// HasAdminKey reports whether orgID has an enabled, unexpired admin key at now.
func (db *DB) HasAdminKey(ctx context.Context, orgID string, now time.Time) (bool, error) {
	var n int
	err := db.sql.QueryRowContext(ctx, `SELECT count(*) FROM api_keys
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
