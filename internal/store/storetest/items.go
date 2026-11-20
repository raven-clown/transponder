package storetest

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

// Item returns a config item with every field set, for tests.
func Item(kind, id string, position int64) store.ConfigItem {
	return store.ConfigItem{
		OrgID:     store.DefaultOrg,
		Kind:      kind,
		ID:        id,
		Position:  position,
		Body:      []byte(`{"type":"openai","base_url":"https://api.example.com/v1"}`),
		Secret:    "v1.sealed",
		Version:   1,
		Source:    store.SourceAPI,
		CreatedAt: time.UnixMilli(1_700_000_000_000),
		UpdatedAt: time.UnixMilli(1_700_000_000_000),
	}
}

func testConfigItems(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	items := []store.ConfigItem{Item("routes", "b", 2000), Item("routes", "a", 2000), Item("routes", "z", 1000), Item("providers", "openai", 0)}
	other := Item("routes", "other-org", 0)
	other.OrgID = "acme"
	for _, it := range append(items, other) {
		if err := db.CreateConfigItem(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CreateConfigItem(ctx, Item("routes", "a", 0)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("same id: err = %v, want ErrConflict", err)
	}
	routes, err := db.ListConfigItems(ctx, store.DefaultOrg, "routes")
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range routes {
		ids = append(ids, it.ID)
	}
	if want := []string{"z", "a", "b"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids %v, want %v", ids, want)
	}
	if !reflect.DeepEqual(routes[1], Item("routes", "a", 2000)) {
		t.Fatalf("got %+v\nwant %+v", routes[1], Item("routes", "a", 2000))
	}
	got, err := db.ConfigItem(ctx, store.DefaultOrg, "providers", "openai")
	if err != nil || !reflect.DeepEqual(got, Item("providers", "openai", 0)) {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := db.ConfigItem(ctx, store.DefaultOrg, "routes", "openai"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("other kind: err = %v, want ErrNotFound", err)
	}
	if none, err := db.ListConfigItems(ctx, store.DefaultOrg, "budgets"); err != nil || len(none) != 0 {
		t.Fatalf("budgets %+v, %v", none, err)
	}
}

func testUpdateConfigItem(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	if err := db.CreateConfigItem(ctx, Item("providers", "p", 0)); err != nil {
		t.Fatal(err)
	}
	it := Item("providers", "p", 5)
	it.Body = []byte(`{"type":"ollama"}`)
	it.Secret = ""
	it.Source = store.SourceFile
	it.UpdatedAt = time.UnixMilli(1_800_000_000_000)
	got, err := db.UpdateConfigItem(ctx, it, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := it
	want.Version = 2
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if _, err := db.UpdateConfigItem(ctx, it, 1); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("stale: err = %v", err)
	}
	if got, err := db.UpdateConfigItem(ctx, it, 0); err != nil || got.Version != 3 {
		t.Fatalf("no version: %+v, %v", got, err)
	}
	if _, err := db.UpdateConfigItem(ctx, Item("providers", "missing", 0), 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing: err = %v", err)
	}
	if err := db.DeleteConfigItem(ctx, store.DefaultOrg, "providers", "p", 2); !errors.Is(err, store.ErrVersionConflict) {
		t.Fatalf("stale delete: err = %v", err)
	}
	if err := db.DeleteConfigItem(ctx, store.DefaultOrg, "providers", "p", 3); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteConfigItem(ctx, store.DefaultOrg, "providers", "p", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted twice: err = %v", err)
	}
}

func testConfigItemsJoinUpdateConfig(t *testing.T, open Opener) {
	db := open(t)
	ctx := context.Background()
	boom := errors.New("boom")
	_, err := db.UpdateConfig(ctx, func(ctx context.Context) error {
		if err := db.CreateConfigItem(ctx, Item("budgets", "b1", 0)); err != nil {
			return err
		}
		if _, err := db.ConfigItem(ctx, store.DefaultOrg, "budgets", "b1"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := db.ConfigItem(ctx, store.DefaultOrg, "budgets", "b1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rolled back item: err = %v", err)
	}
	if _, err := db.UpdateConfig(ctx, func(ctx context.Context) error {
		if err := db.CreateConfigItem(ctx, Item("budgets", "b1", 0)); err != nil {
			return err
		}
		it := Item("budgets", "b1", 7)
		it.UpdatedAt = time.UnixMilli(1_750_000_000_000)
		if _, err := db.UpdateConfigItem(ctx, it, 1); err != nil {
			return err
		}
		list, err := db.ListConfigItems(ctx, store.DefaultOrg, "budgets")
		if err != nil || len(list) != 1 || list[0].Position != 7 {
			return errors.New("the transaction does not see its own writes")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
