package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/raven-clown/transponder/internal/store"
)

var _ store.KeyStore = (*DB)(nil)

// uniqueViolation is the SQLSTATE of a broken primary key or unique constraint.
const uniqueViolation = "23505"

// CreateKey stores a new API key.
func (db *DB) CreateKey(ctx context.Context, k store.APIKey) error {
	teams, err := json.Marshal(nonNil(k.Teams))
	if err != nil {
		return err
	}
	_, err = db.q(ctx).Exec(ctx, `INSERT INTO api_keys
		(org_id, name, hash, last4, role, teams, disabled, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)`,
		k.OrgID, k.Name, k.Hash[:], k.Last4, k.Role, teams, k.Disabled,
		nullMillis(k.ExpiresAt), k.CreatedAt.UnixMilli())
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == uniqueViolation {
		return fmt.Errorf("key %q: %w", k.Name, store.ErrConflict)
	}
	return err
}

// KeyByHash returns the key whose secret hashes to hash.
func (db *DB) KeyByHash(ctx context.Context, hash [32]byte) (store.APIKey, error) {
	var (
		k       store.APIKey
		h       []byte
		teams   []byte
		expires *int64
		created int64
	)
	err := db.q(ctx).QueryRow(ctx, `SELECT org_id, name, hash, last4, role, teams, disabled, expires_at, created_at
		FROM api_keys WHERE hash = $1`, hash[:]).
		Scan(&k.OrgID, &k.Name, &h, &k.Last4, &k.Role, &teams, &k.Disabled, &expires, &created)
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
