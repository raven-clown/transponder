package sqlite

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

func record(i int64, org string) store.UsageRecord {
	return store.UsageRecord{
		OrgID:            org,
		Time:             time.UnixMilli(1_700_000_000_000 + i*1000),
		RequestID:        "req",
		User:             "alice",
		Teams:            []string{"platform"},
		Key:              "alice-key",
		Project:          "demo",
		Route:            "claude-*",
		Provider:         "echo",
		Model:            "m",
		Endpoint:         "chat.completions",
		InputTokens:      10 + i,
		OutputTokens:     20,
		CacheReadTokens:  3,
		CacheWriteTokens: 4,
		CostNUSD:         4542000,
		ReasoningTokens:  7,
		Estimated:        true,
		Status:           200,
		Latency:          150 * time.Millisecond,
		Fallbacks:        1,
	}
}

func TestRecordAndRecentUsage(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	for i := int64(1); i <= 3; i++ {
		if err := db.RecordUsage(ctx, record(i, store.DefaultOrg)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordUsage(ctx, record(9, "acme")); err != nil {
		t.Fatal(err)
	}
	got, err := db.RecentUsage(ctx, store.DefaultOrg, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.UsageRecord{record(3, store.DefaultOrg), record(2, store.DefaultOrg)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestRecordUsageUnderLoad(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	const n = 500
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs <- db.RecordUsage(ctx, record(int64(i), store.DefaultOrg)) })
	}
	wg.Wait()
	close(errs)
	failed := 0
	for err := range errs {
		if err != nil {
			failed++
			t.Log(err)
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d writes failed", failed, n)
	}
	recs, err := db.RecentUsage(ctx, store.DefaultOrg, n+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != n {
		t.Fatalf("records = %d, want %d", len(recs), n)
	}
}

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
