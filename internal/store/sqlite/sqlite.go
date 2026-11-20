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
	"runtime"

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
	// keyByHash is prepared once, so the lookup every call makes is not parsed each time.
	keyByHash *sql.Stmt
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
	// Every call with an API key reads here. With the default of two idle connections the pool
	// closed most after each read and opened new ones, each parsing the schema again.
	conns := max(4, runtime.GOMAXPROCS(0))
	r.SetMaxOpenConns(conns)
	r.SetMaxIdleConns(conns)
	kb, err := r.PrepareContext(ctx, `SELECT `+keyColumns+` FROM api_keys WHERE hash = ?`)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("preparing the key lookup: %w", err), r.Close(), w.Close())
	}
	return &DB{sql: r, w: w, keyByHash: kb}, nil
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
	return errors.Join(db.keyByHash.Close(), db.sql.Close(), db.w.Close())
}
