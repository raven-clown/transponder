package sqlite

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

func waitingUsage(db *DB) int {
	db.usage.mu.Lock()
	defer db.usage.mu.Unlock()
	return len(db.usage.pending)
}

// TestUsageWritesShareATransaction holds SQLite's one writer while 64 calls record usage, then
// lets go. The calls that queued meanwhile must go in together, not one transaction each, and
// every record must be stored.
func TestUsageWritesShareATransaction(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	const calls = 64
	holding, release := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = db.UpdateConfig(ctx, func(context.Context) error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding
	var wg sync.WaitGroup
	for i := range calls {
		wg.Go(func() {
			r := store.UsageRecord{OrgID: store.DefaultOrg, Time: time.Now(), RequestID: "r", Endpoint: "chat", InputTokens: int64(i), Status: 200}
			if err := db.RecordUsage(ctx, r); err != nil {
				t.Error(err)
			}
		})
	}
	// One call leads and waits for the writer; the others queue behind it.
	for waitingUsage(db) < calls-1 {
		runtime.Gosched()
	}
	close(release)
	wg.Wait()
	if n := db.usageBatches.Load(); n > 2 {
		t.Errorf("64 queued records took %d transactions, want at most 2", n)
	}
	recs, err := db.RecentUsage(ctx, store.DefaultOrg, 2*calls)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int64]bool{}
	for _, r := range recs {
		seen[r.InputTokens] = true
	}
	if len(recs) != calls || len(seen) != calls {
		t.Fatalf("stored %d records, %d different, want %d", len(recs), len(seen), calls)
	}
}

// TestUsageWriteErrorReachesEveryCaller closes the store under queued writes: every call must
// get the error, none may hang.
func TestUsageWriteErrorReachesEveryCaller(t *testing.T) {
	db := openTemp(t)
	if err := db.w.Close(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := db.RecordUsage(context.Background(), store.UsageRecord{OrgID: store.DefaultOrg, Time: time.Now()}); err == nil {
				t.Error("want an error from a closed store")
			}
		})
	}
	wg.Wait()
}

// TestUsageInTransaction records usage inside a config transaction, where it joins that
// transaction instead of the batch.
func TestUsageInTransaction(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	_, err := db.UpdateConfig(ctx, func(ctx context.Context) error {
		return db.RecordUsage(ctx, store.UsageRecord{OrgID: store.DefaultOrg, Time: time.Now(), RequestID: "in-tx"})
	})
	if err != nil {
		t.Fatal(err)
	}
	recs, err := db.RecentUsage(ctx, store.DefaultOrg, 10)
	if err != nil || len(recs) != 1 || recs[0].RequestID != "in-tx" {
		t.Fatalf("records %+v, %v", recs, err)
	}
}
