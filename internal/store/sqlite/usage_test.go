package sqlite

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/raven-clown/transponder/internal/store"
)

func TestMigrationKeepsUsage(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	init, err := fs.ReadFile(migrations, "migrations/0001_init.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, raw, fstest.MapFS{"0001_init.sql": {Data: init}}); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(ctx, `INSERT INTO usage (org_id, time, request_id, user, key, endpoint, status)
		VALUES ('default', 1000, 'r1', 'alice@example.com', 'k', 'chat.completions', 200)`); err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	recs, err := db.RecentUsage(ctx, store.DefaultOrg, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].User != "alice@example.com" || recs[0].RequestID != "r1" {
		t.Fatalf("records %+v", recs)
	}
	var n int
	if err := db.sql.QueryRowContext(ctx, `SELECT count(*) FROM pragma_table_info('usage') WHERE name = 'user'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("usage still has a user column (%d, %v)", n, err)
	}
}
