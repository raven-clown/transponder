package sqlite

import (
	"context"
	"io/fs"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
)

func openTemp(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func appliedVersions(t *testing.T, db *DB) []int {
	t.Helper()
	rows, err := db.sql.QueryContext(context.Background(), "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func embeddedVersions(t *testing.T) []int {
	t.Helper()
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	out := make([]int, len(entries))
	for i := range entries {
		out[i] = i + 1
	}
	return out
}

func TestOpenNew(t *testing.T) {
	db := openTemp(t)
	if got := appliedVersions(t, db); !reflect.DeepEqual(got, embeddedVersions(t)) {
		t.Fatalf("applied = %v, want %v", got, embeddedVersions(t))
	}
	if err := db.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestOpenTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	ctx := context.Background()
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if got := appliedVersions(t, db); !reflect.DeepEqual(got, embeddedVersions(t)) {
		t.Fatalf("applied = %v, want each migration once", got)
	}
}

func TestMigrateRollsBack(t *testing.T) {
	db := openTemp(t)
	fsys := fstest.MapFS{
		"0100_good.sql": {Data: []byte("CREATE TABLE good (id INTEGER);")},
		"0101_bad.sql":  {Data: []byte("CREATE TABLE half (id INTEGER); THIS IS NOT SQL;")},
	}
	err := migrate(context.Background(), db.w, fsys)
	if err == nil || !strings.Contains(err.Error(), "0101_bad.sql") {
		t.Fatalf("err = %v, want it to name 0101_bad.sql", err)
	}
	got := appliedVersions(t, db)
	if got[len(got)-1] != 100 {
		t.Fatalf("applied = %v, want last version 100", got)
	}
	var n int
	if err := db.sql.QueryRowContext(context.Background(), "SELECT count(*) FROM sqlite_master WHERE name = 'half'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("table from the failed migration was left behind")
	}
}

func TestPingAfterClose(t *testing.T) {
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := db.Ping(context.Background()); err == nil {
		t.Fatal("Ping after Close succeeded")
	}
}
