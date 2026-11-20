package postgres

import (
	"context"
	"errors"
	"io/fs"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5"

	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/postgres/pgtest"
	"github.com/raven-clown/transponder/internal/store/storetest"
)

func openAt(t *testing.T, dsn string) *DB {
	t.Helper()
	db, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func openTemp(t *testing.T) *DB {
	t.Helper()
	return openAt(t, pgtest.DSN(t))
}

func TestStore(t *testing.T) {
	pgtest.DSN(t) // skips before the suite starts when there is no server
	storetest.Run(t, func(t *testing.T) storetest.Store { return openTemp(t) })
}

func TestMigrationsExpandAndContract(t *testing.T) {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	storetest.CheckMigrations(t, sub)
}

func appliedVersions(t *testing.T, db *DB) []int {
	t.Helper()
	rows, err := db.pool.Query(context.Background(), "SELECT version FROM schema_migrations ORDER BY version")
	if err != nil {
		t.Fatal(err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[int])
	if err != nil {
		t.Fatal(err)
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

func TestOpenTwice(t *testing.T) {
	dsn := pgtest.DSN(t)
	first := openAt(t, dsn)
	if got := appliedVersions(t, first); !reflect.DeepEqual(got, embeddedVersions(t)) {
		t.Fatalf("applied = %v, want %v", got, embeddedVersions(t))
	}
	second := openAt(t, dsn)
	if got := appliedVersions(t, second); !reflect.DeepEqual(got, embeddedVersions(t)) {
		t.Fatalf("applied = %v, want each migration once", got)
	}
}

// Replicas that start together must not apply a migration twice.
func TestOpenTogether(t *testing.T) {
	dsn := pgtest.DSN(t)
	const replicas = 8
	errs := make(chan error, replicas)
	var wg sync.WaitGroup
	for range replicas {
		wg.Go(func() {
			db, err := Open(context.Background(), dsn)
			if err == nil {
				err = db.Close()
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := appliedVersions(t, openAt(t, dsn)); !reflect.DeepEqual(got, embeddedVersions(t)) {
		t.Fatalf("applied = %v, want each migration once", got)
	}
}

// What one replica writes, another reads.
func TestReplicasShareKeysAndUsage(t *testing.T) {
	dsn := pgtest.DSN(t)
	a, b := openAt(t, dsn), openAt(t, dsn)
	ctx := context.Background()
	k := storetest.Key("shared", "member")
	if err := a.CreateKey(ctx, k); err != nil {
		t.Fatal(err)
	}
	if got, err := b.KeyByHash(ctx, k.Hash); err != nil || got.Name != "shared" {
		t.Fatalf("key on the other replica = %+v, %v", got, err)
	}
	if err := b.CreateKey(ctx, k); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if err := a.RecordUsage(ctx, storetest.Record(1, store.DefaultOrg)); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordUsage(ctx, storetest.Record(2, store.DefaultOrg)); err != nil {
		t.Fatal(err)
	}
	for _, db := range []*DB{a, b} {
		recs, err := db.RecentUsage(ctx, store.DefaultOrg, 10)
		if err != nil || len(recs) != 2 {
			t.Fatalf("records = %+v, %v", recs, err)
		}
	}
}

func TestMigrateRollsBack(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	fsys := fstest.MapFS{
		"0100_good.sql": {Data: []byte("CREATE TABLE good (id INTEGER); CREATE TABLE good2 (id INTEGER);")},
		"0101_bad.sql":  {Data: []byte("CREATE TABLE half (id INTEGER); THIS IS NOT SQL;")},
	}
	err := migrate(ctx, db.pool, fsys)
	if err == nil || !strings.Contains(err.Error(), "0101_bad.sql") {
		t.Fatalf("err = %v, want it to name 0101_bad.sql", err)
	}
	got := appliedVersions(t, db)
	if got[len(got)-1] != 100 {
		t.Fatalf("applied = %v, want last version 100", got)
	}
	var tables []string
	rows, err := db.pool.Query(ctx, "SELECT table_name::text FROM information_schema.tables WHERE table_schema = current_schema() AND table_name IN ('good', 'good2', 'half') ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	if tables, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(tables, []string{"good", "good2"}) {
		t.Fatalf("tables = %v, want the failed migration's table gone", tables)
	}
	// The failed run must have given the lock back, or this would wait for ever.
	if err := migrate(ctx, db.pool, fstest.MapFS{}); err != nil {
		t.Fatal(err)
	}
}

func TestMigrateRejectsBadName(t *testing.T) {
	db := openTemp(t)
	err := migrate(context.Background(), db.pool, fstest.MapFS{"init.sql": {Data: []byte("SELECT 1")}})
	if err == nil || !strings.Contains(err.Error(), "init.sql") {
		t.Fatalf("err = %v, want it to name init.sql", err)
	}
}

func TestPingAfterClose(t *testing.T) {
	db, err := Open(context.Background(), pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if err := db.Ping(context.Background()); err == nil {
		t.Fatal("Ping after Close succeeded")
	}
}

// These need no server, so they also run where Postgres is not set up.
func TestOpenErrors(t *testing.T) {
	tests := []struct {
		name, dsn, want string
	}{
		{"not a url", "postgres://%zz", "reading store.postgres"},
		{"nothing listening", "postgres://u:secret@127.0.0.1:1/db?connect_timeout=2", "opening postgres"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Open(context.Background(), tt.dsn)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("the error shows the password: %v", err)
			}
		})
	}
}
