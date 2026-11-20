package postgres

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationLock is the advisory lock replicas take while migrating, so two that start together
// do not apply the same migration twice.
const migrationLock int64 = 0x7472616e73706f6e // "transpon"

func migrate(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("opening postgres: %w", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLock); err != nil {
		return fmt.Errorf("locking for migrations: %w", err)
	}
	defer func() {
		// The lock belongs to the session, so a connection that cannot give it back is closed
		// rather than returned to the pool still holding it.
		if _, err := conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLock); err != nil {
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
		}
	}()

	if _, err := conn.Exec(ctx, "CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at BIGINT NOT NULL)"); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	rows, err := conn.Query(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("reading schema_migrations: %w", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		return fmt.Errorf("reading schema_migrations: %w", err)
	}
	applied := map[int]bool{}
	for _, v := range versions {
		applied[v] = true
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
		if err := apply(ctx, conn, f.version, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", f.name, err)
		}
	}
	return nil
}

func apply(ctx context.Context, conn *pgxpool.Conn, version int, body string) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	// With no arguments the file goes to the server as one simple query, so it may hold several
	// statements.
	if _, err := tx.Exec(ctx, body); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES ($1, extract(epoch from now())::bigint)", version); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return tx.Commit(ctx)
}
