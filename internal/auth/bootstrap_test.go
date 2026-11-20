package auth

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/store"
	"github.com/raven-clown/transponder/internal/store/sqlite"
)

func TestEnsureAdminOnce(t *testing.T) {
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := func() time.Time { return testNow }

	var out bytes.Buffer
	if err := EnsureAdmin(ctx, db, now, &out); err != nil {
		t.Fatal(err)
	}
	var plain string
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(line, "tp_") {
			plain = line
		}
	}
	if plain == "" {
		t.Fatalf("no key printed: %q", out.String())
	}
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil)
	r.Header.Set("Authorization", "Bearer "+plain)
	id, err := NewAuthenticator(db, now).Authenticate(r)
	if err != nil {
		t.Fatal(err)
	}
	if id.Role != RoleAdmin || id.User != "admin" {
		t.Fatalf("identity = %+v", id)
	}

	out.Reset()
	if err := EnsureAdmin(ctx, db, now, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("second call printed %q", out.String())
	}
}

func TestEnsureAdminNameTaken(t *testing.T) {
	keys := &fakeKeys{}
	setupKey(t, keys, "admin", func(k *store.APIKey) { k.Role = string(RoleAdmin); k.Disabled = true })
	var out bytes.Buffer
	if err := EnsureAdmin(context.Background(), keys, func() time.Time { return testNow }, &out); err != nil {
		t.Fatal(err)
	}
	created := keys.keys[len(keys.keys)-1]
	if created.Name != "admin-"+created.Last4 || created.Role != string(RoleAdmin) || created.Disabled {
		t.Fatalf("created = %+v", created)
	}
}
