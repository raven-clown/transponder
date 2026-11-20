package sqlite_test

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
)

func benchDB(b *testing.B) *sqlite.DB {
	db, err := sqlite.Open(context.Background(), filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	return db
}

func benchRecord() store.UsageRecord {
	return store.UsageRecord{
		OrgID: store.DefaultOrg, Time: time.Now(), RequestID: "req-bench", User: "ana", Teams: []string{"platform"},
		Key: "bench", Route: "gpt-4o-mini", Provider: "openai", Model: "gpt-4o-mini", Endpoint: "chat",
		InputTokens: 420, OutputTokens: 256, CostNUSD: 650_000, Status: 200, Latency: 20 * time.Millisecond,
	}
}

// BenchmarkRecordUsage is the write every call makes when it ends.
func BenchmarkRecordUsage(b *testing.B) {
	db := benchDB(b)
	ctx := context.Background()
	rec := benchRecord()
	b.Run("serial", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := db.RecordUsage(ctx, rec); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("parallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if err := db.RecordUsage(ctx, rec); err != nil {
					b.Error(err)
					return
				}
			}
		})
	})
}

// BenchmarkKeyByHash is the read that authenticates every call made with an API key.
func BenchmarkKeyByHash(b *testing.B) {
	db := benchDB(b)
	ctx := context.Background()
	hash := sha256.Sum256([]byte("tp-bench"))
	if err := db.CreateKey(ctx, store.APIKey{OrgID: store.DefaultOrg, Name: "bench", Hash: hash, Last4: "ench", Role: "member", CreatedAt: time.Now()}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := db.KeyByHash(ctx, hash); err != nil {
				b.Error(err)
				return
			}
		}
	})
}
