package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/storetest"
)

// Another process holding the write lock, such as a backup or a second transponder on the same
// file, makes a write wait for up to busy_timeout rather than fail at once.
func TestWriteWaitsForALockedDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "t.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	other, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	conn, err := other.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- db.RecordUsage(ctx, storetest.Record(1, store.DefaultOrg)) }()
	select {
	case err := <-done:
		_, _ = conn.ExecContext(ctx, "ROLLBACK")
		t.Fatalf("the write did not wait for the lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the write after the lock was released: %v", err)
	}
	if recs, err := db.RecentUsage(ctx, store.DefaultOrg, 10); err != nil || len(recs) != 1 {
		t.Fatalf("recent usage %d, %v", len(recs), err)
	}
}

// A full disk fails each write with an error the caller sees and counts; nothing already stored
// is lost, reads go on, and writes work again once there is room.
func TestWriteFailsWhenTheDatabaseIsFull(t *testing.T) {
	ctx := context.Background()
	db := openTemp(t)
	if err := db.RecordUsage(ctx, storetest.Record(1, store.DefaultOrg)); err != nil {
		t.Fatal(err)
	}
	var pages int64
	if err := db.w.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	// The write pool has one connection, so the cap holds for every write.
	if _, err := db.w.ExecContext(ctx, "PRAGMA max_page_count = "+strconv.FormatInt(pages, 10)); err != nil {
		t.Fatal(err)
	}
	var full error
	for i := int64(2); i < 5000 && full == nil; i++ {
		full = db.RecordUsage(ctx, storetest.Record(i, store.DefaultOrg))
	}
	if full == nil || !strings.Contains(full.Error(), "full") {
		t.Fatalf("err = %v, want the database to be full", full)
	}
	if recs, err := db.RecentUsage(ctx, store.DefaultOrg, 1); err != nil || len(recs) != 1 {
		t.Fatalf("reading while full: %d, %v", len(recs), err)
	}
	if _, err := db.w.ExecContext(ctx, "PRAGMA max_page_count = 1000000"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordUsage(ctx, storetest.Record(9999, store.DefaultOrg)); err != nil {
		t.Fatalf("a write once there is room again: %v", err)
	}
}
