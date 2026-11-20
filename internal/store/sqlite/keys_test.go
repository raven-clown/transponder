package sqlite

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

func key(name, role string) store.APIKey {
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

func TestCreateAndFindKey(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	want := key("logbook", "member")
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
}

func TestCreateKeyConflict(t *testing.T) {
	db := openTemp(t)
	ctx := context.Background()
	if err := db.CreateKey(ctx, key("a", "member")); err != nil {
		t.Fatal(err)
	}
	dup := key("a", "member")
	dup.Hash = sha256.Sum256([]byte("other"))
	if err := db.CreateKey(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestKeyByHashNotFound(t *testing.T) {
	db := openTemp(t)
	if _, err := db.KeyByHash(context.Background(), sha256.Sum256([]byte("x"))); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestHasAdminKey(t *testing.T) {
	now := time.UnixMilli(1_750_000_000_000)
	tests := []struct {
		name string
		keys []store.APIKey
		org  string
		want bool
	}{
		{name: "empty", org: store.DefaultOrg, want: false},
		{name: "member only", keys: []store.APIKey{key("m", "member")}, org: store.DefaultOrg, want: false},
		{name: "disabled admin", keys: []store.APIKey{func() store.APIKey { k := key("a", "admin"); k.Disabled = true; return k }()}, org: store.DefaultOrg, want: false},
		{name: "expired admin", keys: []store.APIKey{func() store.APIKey { k := key("a", "admin"); k.ExpiresAt = now.Add(-time.Second); return k }()}, org: store.DefaultOrg, want: false},
		{name: "admin expiring later", keys: []store.APIKey{func() store.APIKey { k := key("a", "admin"); k.ExpiresAt = now.Add(time.Hour); return k }()}, org: store.DefaultOrg, want: true},
		{name: "enabled admin", keys: []store.APIKey{key("a", "admin")}, org: store.DefaultOrg, want: true},
		{name: "other org", keys: []store.APIKey{key("a", "admin")}, org: "acme", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openTemp(t)
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
