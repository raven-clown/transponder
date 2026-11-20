// Package storetest is the test suite every store implementation must pass, so SQLite and
// Postgres behave the same behind the store interfaces.
package storetest

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

// Store is what the suite tests.
type Store interface {
	store.KeyStore
	store.UsageStore
	Ping(ctx context.Context) error
}

// Opener returns an empty store that lives until the test ends.
type Opener func(t *testing.T) Store

// Run runs every test of the suite against stores from open.
func Run(t *testing.T, open Opener) {
	tests := []struct {
		name string
		fn   func(*testing.T, Opener)
	}{
		{"Ping", testPing},
		{"CreateAndFindKey", testCreateAndFindKey},
		{"CreateKeyConflict", testCreateKeyConflict},
		{"KeyByHashNotFound", testKeyByHashNotFound},
		{"HasAdminKey", testHasAdminKey},
		{"RecordAndRecentUsage", testRecordAndRecentUsage},
		{"RecordUsageUnderLoad", testRecordUsageUnderLoad},
		{"UsageSince", testUsageSince},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { tt.fn(t, open) })
	}
}

func testPing(t *testing.T, open Opener) {
	if err := open(t).Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Key returns a key with every field set, for tests.
func Key(name, role string) store.APIKey {
	return store.APIKey{
		OrgID:     store.DefaultOrg,
		Name:      name,
		Hash:      sha256.Sum256([]byte(name)),
		Last4:     "ab12",
		Role:      role,
		Teams:     []string{"platform", "ml"},
		CreatedAt: time.UnixMilli(1_700_000_000_000),
	}
}

func testCreateAndFindKey(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	want := Key("logbook", "member")
	want.ExpiresAt = time.UnixMilli(1_800_000_000_000)
	if err := db.CreateKey(ctx, want); err != nil {
		t.Fatal(err)
	}
	got, err := db.KeyByHash(ctx, want.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	noTeams := Key("bare", "admin")
	noTeams.Teams = nil
	noTeams.Disabled = true
	if err := db.CreateKey(ctx, noTeams); err != nil {
		t.Fatal(err)
	}
	if got, err := db.KeyByHash(ctx, noTeams.Hash); err != nil || !reflect.DeepEqual(got, noTeams) {
		t.Fatalf("got %+v, %v\nwant %+v", got, err, noTeams)
	}
}

func testCreateKeyConflict(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	if err := db.CreateKey(ctx, Key("a", "member")); err != nil {
		t.Fatal(err)
	}
	sameName := Key("a", "member")
	sameName.Hash = sha256.Sum256([]byte("other"))
	if err := db.CreateKey(ctx, sameName); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("same name: err = %v, want ErrConflict", err)
	}
	sameHash := Key("a", "member")
	sameHash.Name = "b"
	if err := db.CreateKey(ctx, sameHash); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("same hash: err = %v, want ErrConflict", err)
	}
}

func testKeyByHashNotFound(t *testing.T, open Opener) {
	db := open(t)
	if _, err := db.KeyByHash(context.Background(), sha256.Sum256([]byte("x"))); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func testHasAdminKey(t *testing.T, open Opener) {
	now := time.UnixMilli(1_750_000_000_000)
	with := func(name string, edit func(*store.APIKey)) store.APIKey {
		k := Key(name, "admin")
		edit(&k)
		return k
	}
	tests := []struct {
		name string
		keys []store.APIKey
		org  string
		want bool
	}{
		{name: "empty", org: store.DefaultOrg, want: false},
		{name: "member only", keys: []store.APIKey{Key("m", "member")}, org: store.DefaultOrg, want: false},
		{name: "disabled admin", keys: []store.APIKey{with("a", func(k *store.APIKey) { k.Disabled = true })}, org: store.DefaultOrg, want: false},
		{name: "expired admin", keys: []store.APIKey{with("a", func(k *store.APIKey) { k.ExpiresAt = now.Add(-time.Second) })}, org: store.DefaultOrg, want: false},
		{name: "admin expiring later", keys: []store.APIKey{with("a", func(k *store.APIKey) { k.ExpiresAt = now.Add(time.Hour) })}, org: store.DefaultOrg, want: true},
		{name: "enabled admin", keys: []store.APIKey{Key("a", "admin")}, org: store.DefaultOrg, want: true},
		{name: "other org", keys: []store.APIKey{Key("a", "admin")}, org: "acme", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := open(t)
			ctx := context.Background()
			for _, k := range tt.keys {
				if err := db.CreateKey(ctx, k); err != nil {
					t.Fatal(err)
				}
			}
			got, err := db.HasAdminKey(ctx, tt.org, now)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("HasAdminKey = %v, want %v", got, tt.want)
			}
		})
	}
}

// Record returns a usage record with every field set, for tests.
func Record(i int64, org string) store.UsageRecord {
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

func testRecordAndRecentUsage(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	for i := int64(1); i <= 3; i++ {
		if err := db.RecordUsage(ctx, Record(i, store.DefaultOrg)); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordUsage(ctx, Record(9, "acme")); err != nil {
		t.Fatal(err)
	}
	got, err := db.RecentUsage(ctx, store.DefaultOrg, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := []store.UsageRecord{Record(3, store.DefaultOrg), Record(2, store.DefaultOrg)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func testRecordUsageUnderLoad(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	const n = 500
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { errs <- db.RecordUsage(ctx, Record(int64(i), store.DefaultOrg)) })
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

func testUsageSince(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	base := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)
	for _, r := range []store.UsageRecord{
		{OrgID: store.DefaultOrg, Time: base.AddDate(0, -1, 0), User: "old", CostNUSD: 1},
		{OrgID: store.DefaultOrg, Time: base, User: "alice", Teams: []string{"t"}, Key: "k", InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheWriteTokens: 4, ReasoningTokens: 5, CostNUSD: 6},
		{OrgID: "other", Time: base, User: "bob", CostNUSD: 1},
	} {
		if err := db.RecordUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	var got []store.UsageRecord
	err := db.UsageSince(ctx, store.DefaultOrg, base.Add(-time.Hour), func(r store.UsageRecord) error {
		got = append(got, r)
		return nil
	})
	if err != nil || len(got) != 1 {
		t.Fatalf("records %+v %v", got, err)
	}
	r := got[0]
	if r.User != "alice" || r.Key != "k" || len(r.Teams) != 1 || r.InputTokens+r.OutputTokens+r.CacheReadTokens+r.CacheWriteTokens+r.ReasoningTokens != 15 || r.CostNUSD != 6 || !r.Time.Equal(base) {
		t.Fatalf("record %+v", r)
	}
	stop := errors.New("stop")
	if err := db.UsageSince(ctx, store.DefaultOrg, time.Time{}, func(store.UsageRecord) error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("err = %v, want the callback's error", err)
	}
}
