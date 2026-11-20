// Package postgres implements the store on a Postgres database, which several replicas can share.
package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// DB is a transponder store backed by a Postgres database.
type DB struct {
	pool *pgxpool.Pool
}

// Open connects to the database named by dsn, a postgres:// URL or a keyword=value string, and
// applies pending migrations. Pool settings such as pool_max_conns are read from dsn.
func Open(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("reading store.postgres: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("opening postgres: %w", err)
	}
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		pool.Close()
		return nil, err
	}
	if err := migrate(ctx, pool, sub); err != nil {
		pool.Close()
		return nil, err
	}
	return &DB{pool: pool}, nil
}

// Ping checks that the database answers.
func (db *DB) Ping(ctx context.Context) error {
	return db.pool.Ping(ctx)
}

// Close closes every connection to the database.
func (db *DB) Close() error {
	db.pool.Close()
	return nil
}
