package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/raven-clown/transponder/internal/store"
)

var _ store.ConfigStore = (*DB)(nil)

// querier is what both the pool and a transaction offer.
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type txKey struct{ db *DB }

// q returns the transaction ctx carries for this database, or the pool. Store methods use it
// so their writes and reads join an UpdateConfig.
func (db *DB) q(ctx context.Context) querier {
	if tx, ok := ctx.Value(txKey{db}).(pgx.Tx); ok {
		return tx
	}
	return db.pool
}

// ConfigVersion implements store.ConfigStore.
func (db *DB) ConfigVersion(ctx context.Context) (int64, error) {
	var v int64
	err := db.q(ctx).QueryRow(ctx, `SELECT version FROM config_version WHERE org_id = $1`, store.DefaultOrg).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("reading config_version: %w", err)
	}
	return v, nil
}

// UpdateConfig implements store.ConfigStore. Called inside another UpdateConfig it joins that
// transaction. The config_version row is locked before fn runs, so config writers run one at a
// time and each sees what the ones before it committed: a check fn makes, such as that no
// route uses a provider it deletes, still holds when it commits.
func (db *DB) UpdateConfig(ctx context.Context, fn func(ctx context.Context) error) (int64, error) {
	if _, ok := ctx.Value(txKey{db}).(pgx.Tx); ok {
		if err := fn(ctx); err != nil {
			return 0, err
		}
		return db.bump(ctx)
	}
	tx, err := db.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT version FROM config_version WHERE org_id = $1 FOR UPDATE`, store.DefaultOrg); err != nil {
		return 0, fmt.Errorf("locking config_version: %w", err)
	}
	ctx = context.WithValue(ctx, txKey{db}, tx)
	if err := fn(ctx); err != nil {
		return 0, err
	}
	v, err := db.bump(ctx)
	if err != nil {
		return 0, err
	}
	return v, tx.Commit(ctx)
}

func (db *DB) bump(ctx context.Context) (int64, error) {
	var v int64
	err := db.q(ctx).QueryRow(ctx, `UPDATE config_version SET version = version + 1, updated_at = $1
		WHERE org_id = $2 RETURNING version`, time.Now().UnixMilli(), store.DefaultOrg).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("raising config_version: %w", err)
	}
	return v, nil
}
