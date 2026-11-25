package storetest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

func testListAndFindKeys(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	for _, k := range []store.APIKey{Key("zed", "member"), Key("alpha", "admin"), Key("Mid", "viewer")} {
		if err := db.CreateKey(ctx, k); err != nil {
			t.Fatal(err)
		}
	}
	other := Key("other", "admin")
	other.OrgID = "acme"
	if err := db.CreateKey(ctx, other); err != nil {
		t.Fatal(err)
	}
	keys, err := db.ListKeys(ctx, store.DefaultOrg)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, k := range keys {
		names = append(names, k.Name)
	}
	if want := []string{"Mid", "alpha", "zed"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names %v, want %v", names, want)
	}
	got, err := db.KeyByName(ctx, store.DefaultOrg, "alpha")
	if err != nil || !reflect.DeepEqual(got, Key("alpha", "admin")) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := db.KeyByName(ctx, store.DefaultOrg, "other"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other org: err = %v, want ErrNotFound", err)
	}
	file := Key("from-file", "member")
	file.Source = store.SourceFile
	if err := db.CreateKey(ctx, file); err != nil {
		t.Fatal(err)
	}
	if got, err := db.KeyByName(ctx, store.DefaultOrg, "from-file"); err != nil || got.Source != store.SourceFile {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func testUpdateKey(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	if err := db.CreateKey(ctx, Key("k", "member")); err != nil {
		t.Fatal(err)
	}
	if err := db.CreateKey(ctx, Key("taken", "member")); err != nil {
		t.Fatal(err)
	}
	k := Key("k", "admin")
	k.Teams = nil
	k.Disabled = true
	k.ExpiresAt = time.UnixMilli(1_900_000_000_000)
	k.Hash = sha256.Sum256([]byte("rotated"))
	k.Last4 = "zz99"
	k.UpdatedAt = time.UnixMilli(1_800_000_000_000)
	got, err := db.UpdateKey(ctx, k, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := k
	want.Version = 2
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if _, err := db.UpdateKey(ctx, k, 1); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("stale version: err = %v, want ErrVersionConflict", err)
	}
	if got, err := db.UpdateKey(ctx, k, 0); err != nil || got.Version != 3 {
		t.Fatalf("no version: got %+v, %v", got, err)
	}
	missing := Key("missing", "member")
	if _, err := db.UpdateKey(ctx, missing, 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: err = %v, want ErrNotFound", err)
	}
	clash := Key("k", "member")
	clash.Hash = Key("taken", "member").Hash
	if _, err := db.UpdateKey(ctx, clash, 0); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("same hash: err = %v, want ErrConflict", err)
	}
	if found, err := db.KeyByHash(ctx, sha256.Sum256([]byte("rotated"))); err != nil || found.Name != "k" {
		t.Fatalf("by new hash: %+v, %v", found, err)
	}
	if _, err := db.KeyByHash(ctx, Key("k", "member").Hash); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old hash: err = %v, want ErrNotFound", err)
	}
}

func testDeleteKey(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	for _, name := range []string{"a", "b"} {
		if err := db.CreateKey(ctx, Key(name, "member")); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.DeleteKey(ctx, store.DefaultOrg, "a", 2); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("stale version: err = %v", err)
	}
	if err := db.DeleteKey(ctx, store.DefaultOrg, "a", 1); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteKey(ctx, store.DefaultOrg, "b", 0); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteKey(ctx, store.DefaultOrg, "a", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted twice: err = %v, want ErrNotFound", err)
	}
	if keys, err := db.ListKeys(ctx, store.DefaultOrg); err != nil || len(keys) != 0 {
		t.Fatalf("keys %+v, %v", keys, err)
	}
}

// testKeyWritesJoinUpdateConfig checks that key writes made inside UpdateConfig commit and roll
// back with it, so a write and its audit record never part.
func testKeyWritesJoinUpdateConfig(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	if err := db.CreateKey(ctx, Key("k", "member")); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("boom")
	_, err := db.UpdateConfig(ctx, func(ctx context.Context) error {
		if err := db.CreateKey(ctx, Key("new", "member")); err != nil {
			return err
		}
		k, err := db.KeyByName(ctx, store.DefaultOrg, "k")
		if err != nil {
			return err
		}
		k.Role = "admin"
		if _, err := db.UpdateKey(ctx, k, k.Version); err != nil {
			return err
		}
		if err := db.DeleteKey(ctx, store.DefaultOrg, "new", 0); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	k, err := db.KeyByName(ctx, store.DefaultOrg, "k")
	if err != nil || k.Role != "member" || k.Version != 1 {
		t.Fatalf("rolled back key %+v, %v", k, err)
	}
	if _, err := db.UpdateConfig(ctx, func(ctx context.Context) error {
		return db.DeleteKey(ctx, store.DefaultOrg, "k", 1)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.KeyByName(ctx, store.DefaultOrg, "k"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func testListUsage(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	for i := int64(1); i <= 5; i++ {
		r := Record(i, store.DefaultOrg)
		if i%2 == 0 {
			r.User, r.Key, r.Project = "bob", "bob-key", "other"
		}
		if err := db.RecordUsage(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordUsage(ctx, Record(9, "acme")); err != nil {
		t.Fatal(err)
	}
	all, err := db.ListUsage(ctx, store.UsageQuery{OrgID: store.DefaultOrg, Limit: 10})
	if err != nil || len(all) != 5 {
		t.Fatalf("all: %d records, %v", len(all), err)
	}
	for i := 1; i < len(all); i++ {
		if all[i].ID >= all[i-1].ID {
			t.Fatalf("not newest first: ids %d then %d", all[i-1].ID, all[i].ID)
		}
	}
	want := Record(5, store.DefaultOrg)
	want.ID = all[0].ID
	if !reflect.DeepEqual(all[0], want) {
		t.Fatalf("got %+v\nwant %+v", all[0], want)
	}
	page, err := db.ListUsage(ctx, store.UsageQuery{OrgID: store.DefaultOrg, BeforeID: all[1].ID, Limit: 2})
	if err != nil || len(page) != 2 || page[0].ID != all[2].ID || page[1].ID != all[3].ID {
		t.Fatalf("page %+v, %v", page, err)
	}
	tests := []struct {
		name string
		q    store.UsageQuery
		want int
	}{
		{"user", store.UsageQuery{User: "bob"}, 2},
		{"key", store.UsageQuery{Key: "alice-key"}, 3},
		{"project", store.UsageQuery{Project: "other"}, 2},
		{"from", store.UsageQuery{From: Record(4, "").Time}, 2},
		{"to", store.UsageQuery{To: Record(4, "").Time}, 3},
		{"limit", store.UsageQuery{Limit: 1}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.q.OrgID = store.DefaultOrg
			if tt.q.Limit == 0 {
				tt.q.Limit = 10
			}
			got, err := db.ListUsage(ctx, tt.q)
			if err != nil || len(got) != tt.want {
				t.Fatalf("%d records, %v; want %d", len(got), err, tt.want)
			}
		})
	}
}

// Audit returns an audit record with every field but the id set, for tests.
func Audit(i int64, actor, kind, resource string) store.AuditRecord {
	return store.AuditRecord{
		OrgID:     store.DefaultOrg,
		Time:      time.UnixMilli(1_700_000_000_000 + i*1000),
		Actor:     actor,
		Action:    "update",
		Kind:      kind,
		Resource:  resource,
		Diff:      []byte(`{"before":{"role":"member"},"after":{"role":"admin"}}`),
		RequestID: "req-1",
	}
}

func testAudit(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	recs := []store.AuditRecord{
		Audit(1, "key:admin", "keys", "logbook"),
		Audit(2, "user:alice", "keys", "rosetta"),
		Audit(3, "key:admin", "providers", "openai"),
		Audit(4, "user:alice", "keys", "logbook"),
	}
	for _, r := range recs {
		if err := db.AddAudit(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	other := Audit(5, "key:admin", "keys", "logbook")
	other.OrgID = "acme"
	empty := Audit(6, "key:admin", "keys", "empty")
	empty.Diff = nil
	for _, r := range []store.AuditRecord{other, empty} {
		if err := db.AddAudit(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	all, err := db.ListAudit(ctx, store.AuditQuery{OrgID: store.DefaultOrg, Limit: 10})
	if err != nil || len(all) != 5 {
		t.Fatalf("all: %d records, %v", len(all), err)
	}
	if string(all[0].Diff) != "{}" {
		t.Fatalf("empty diff stored as %q", all[0].Diff)
	}
	got := all[1]
	want := recs[3]
	want.ID = got.ID
	var gotDiff, wantDiff any
	if err := json.Unmarshal(got.Diff, &gotDiff); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(want.Diff, &wantDiff)
	if !reflect.DeepEqual(gotDiff, wantDiff) {
		t.Fatalf("diff %s, want %s", got.Diff, want.Diff)
	}
	got.Diff, want.Diff = nil, nil
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	del := Audit(7, "key:admin", "routes", "chat")
	del.Action = "delete"
	if err := db.AddAudit(ctx, del); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		q     store.AuditQuery
		wants []string
	}{
		{"action", store.AuditQuery{Action: "delete"}, []string{"chat"}},
		{"actor", store.AuditQuery{Actor: "user:alice"}, []string{"logbook", "rosetta"}},
		{"kind", store.AuditQuery{Kind: "providers"}, []string{"openai"}},
		{"kind and resource", store.AuditQuery{Kind: "keys", Resource: "logbook"}, []string{"logbook", "logbook"}},
		{"from and to", store.AuditQuery{From: recs[1].Time, To: recs[3].Time}, []string{"openai", "rosetta"}},
		{"before", store.AuditQuery{BeforeID: all[2].ID, Limit: 1}, []string{"rosetta"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.q.OrgID = store.DefaultOrg
			if tt.q.Limit == 0 {
				tt.q.Limit = 10
			}
			got, err := db.ListAudit(ctx, tt.q)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, r := range got {
				names = append(names, r.Resource)
			}
			if !reflect.DeepEqual(names, tt.wants) {
				t.Fatalf("resources %v, want %v", names, tt.wants)
			}
		})
	}
}

func testAuditRollsBackWithConfig(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	boom := errors.New("boom")
	if _, err := db.UpdateConfig(ctx, func(ctx context.Context) error {
		if err := db.AddAudit(ctx, Audit(1, "key:admin", "keys", "gone")); err != nil {
			return err
		}
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := db.UpdateConfig(ctx, func(ctx context.Context) error {
		return db.AddAudit(ctx, Audit(2, "key:admin", "keys", "kept"))
	}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListAudit(ctx, store.AuditQuery{OrgID: store.DefaultOrg, Limit: 10})
	if err != nil || len(got) != 1 || got[0].Resource != "kept" {
		t.Fatalf("records %+v, %v", got, err)
	}
}
