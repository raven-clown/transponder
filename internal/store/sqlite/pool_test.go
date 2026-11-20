package sqlite

import (
	"context"
	"crypto/sha256"
	"sync"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

// TestReadsReuseConnections looks keys up from many callers at once, twice. Every call with an
// API key does this lookup, so a read pool that closes the connections it has no room to keep
// opens a new SQLite connection, schema read included, for most calls under load.
func TestReadsReuseConnections(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	hash := sha256.Sum256([]byte("tp-pool"))
	if err := db.CreateKey(ctx, store.APIKey{OrgID: store.DefaultOrg, Name: "pool", Hash: hash, Last4: "pool", Role: "member", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		var wg sync.WaitGroup
		for range 32 {
			wg.Go(func() {
				for range 10 {
					if _, err := db.KeyByHash(ctx, hash); err != nil {
						t.Error(err)
						return
					}
				}
			})
		}
		wg.Wait()
	}
	if s := db.sql.Stats(); s.MaxIdleClosed > 0 {
		t.Fatalf("the read pool closed %d connections it had no room to keep (%+v)", s.MaxIdleClosed, s)
	}
}

// TestKeyByHashInTransaction checks that a lookup inside a transaction still sees the
// transaction's own writes, which the prepared statement on the read pool would not.
func TestKeyByHashInTransaction(t *testing.T) {
	db := openTemp(t)
	hash := sha256.Sum256([]byte("tp-tx"))
	_, err := db.UpdateConfig(context.Background(), func(ctx context.Context) error {
		if err := db.CreateKey(ctx, store.APIKey{OrgID: store.DefaultOrg, Name: "tx", Hash: hash, Last4: "p-tx", Role: "member", CreatedAt: time.Now()}); err != nil {
			return err
		}
		k, err := db.KeyByHash(ctx, hash)
		if err != nil || k.Name != "tx" {
			t.Errorf("inside the transaction: %+v, %v", k, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if k, err := db.KeyByHash(context.Background(), hash); err != nil || k.Name != "tx" {
		t.Fatalf("after the transaction: %+v, %v", k, err)
	}
}
