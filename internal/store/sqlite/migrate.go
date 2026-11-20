package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	if _, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL)"); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	applied := map[int]bool{}
	rows, err := db.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("reading schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return err
		}
		applied[v] = true
	}
	if err := rows.Close(); err != nil {
		return err
	}

	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return err
	}
	type file struct {
		version int
		name    string
	}
	var files []file
	for _, n := range names {
		num, _, ok := strings.Cut(n, "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil {
			return fmt.Errorf("migration %s: name must start with a number and an underscore", n)
		}
		files = append(files, file{v, n})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })

	for _, f := range files {
		if applied[f.version] {
			continue
		}
		body, err := fs.ReadFile(fsys, f.name)
		if err != nil {
			return err
		}
		if err := apply(ctx, db, f.version, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", f.name, err)
		}
	}
	return nil
}

func apply(ctx context.Context, db *sql.DB, version int, body string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, body); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES (?, strftime('%s','now'))", version); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
