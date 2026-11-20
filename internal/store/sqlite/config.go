package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

var _ store.ConfigStore = (*DB)(nil)

// querier is what both the write pool and a transaction offer.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type txKey struct{ db *DB }

// writer returns the transaction ctx carries for this database, or the write pool. Every write
// goes through it: the pool has one connection, so a write that bypassed an open transaction
// would wait for it for ever.
func (db *DB) writer(ctx context.Context) querier {
	if tx, ok := ctx.Value(txKey{db}).(*sql.Tx); ok {
		return tx
	}
	return db.w
}

// reader is the transaction when there is one, so a read inside it sees its own writes, and the
// read pool otherwise.
func (db *DB) reader(ctx context.Context) querier {
	if tx, ok := ctx.Value(txKey{db}).(*sql.Tx); ok {
		return tx
	}
	return db.sql
}

// ConfigVersion implements store.ConfigStore.
func (db *DB) ConfigVersion(ctx context.Context) (int64, error) {
	var v int64
	err := db.reader(ctx).QueryRowContext(ctx, `SELECT version FROM config_version WHERE org_id = ?`, store.DefaultOrg).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("reading config_version: %w", err)
	}
	return v, nil
}

// UpdateConfig implements store.ConfigStore. Called inside another UpdateConfig it joins that
// transaction.
func (db *DB) UpdateConfig(ctx context.Context, fn func(ctx context.Context) error) (int64, error) {
	if _, ok := ctx.Value(txKey{db}).(*sql.Tx); ok {
		if err := fn(ctx); err != nil {
			return 0, err
		}
		return db.bump(ctx)
	}
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	ctx = context.WithValue(ctx, txKey{db}, tx)
	if err := fn(ctx); err != nil {
		return 0, err
	}
	v, err := db.bump(ctx)
	if err != nil {
		return 0, err
	}
	return v, tx.Commit()
}

func (db *DB) bump(ctx context.Context) (int64, error) {
	var v int64
	err := db.writer(ctx).QueryRowContext(ctx, `UPDATE config_version SET version = version + 1, updated_at = ?
		WHERE org_id = ? RETURNING version`, time.Now().UnixMilli(), store.DefaultOrg).Scan(&v)
	if err != nil {
		return 0, fmt.Errorf("raising config_version: %w", err)
	}
	return v, nil
}
