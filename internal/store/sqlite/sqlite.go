// Package sqlite implements the store on an embedded SQLite database.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrations embed.FS

// DB is a transponder store backed by one SQLite file.
// SQLite allows one writer at a time, so writes go through a pool with a single
// connection and queue in Go; reads use their own pool and run alongside in WAL mode.
type DB struct {
	sql *sql.DB
	w   *sql.DB
}

// Open opens or creates the database at path and applies pending migrations.
func Open(ctx context.Context, path string) (*DB, error) {
	dsn := "file:" + url.PathEscape(path) + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	w, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	w.SetMaxOpenConns(1)
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := migrate(ctx, w, sub); err != nil {
		_ = w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn+"&mode=ro")
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("opening sqlite: %w", err)
	}
	return &DB{sql: r, w: w}, nil
}

// Ping checks that the database answers.
func (db *DB) Ping(ctx context.Context) error {
	if err := db.w.PingContext(ctx); err != nil {
		return err
	}
	return db.sql.PingContext(ctx)
}

// Close closes the database.
func (db *DB) Close() error {
	return errors.Join(db.sql.Close(), db.w.Close())
}
