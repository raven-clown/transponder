package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/raven-clown/transponder/internal/store"
)

var testNow = time.UnixMilli(1_750_000_000_000)

func setupKey(t *testing.T, keys *fakeKeys, name string, mutate func(*store.APIKey)) string {
	t.Helper()
	plain, hash, last4, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	k := store.APIKey{OrgID: store.DefaultOrg, Name: name, Hash: hash, Last4: last4, Role: string(RoleMember), Teams: []string{"platform"}}
	if mutate != nil {
		mutate(&k)
	}
	if err := keys.CreateKey(context.Background(), k); err != nil {
		t.Fatal(err)
	}
	return plain
}

func TestAuthenticateHeaderForms(t *testing.T) {
	keys := &fakeKeys{}
	plain := setupKey(t, keys, "alice", nil)
	a := NewAuthenticator(keys, func() time.Time { return testNow })
	tests := map[string][2]string{
		"bearer":          {"Authorization", "Bearer " + plain},
		"lowercase":       {"authorization", "bearer " + plain},
		"extra spaces":    {"Authorization", "Bearer   " + plain + "  "},
		"anthropic style": {"x-api-key", plain},
	}
	for name, h := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil)
			r.Header.Set(h[0], h[1])
			id, err := a.Authenticate(r)
			if err != nil {
				t.Fatal(err)
			}
			if id.User != "alice" || id.Key != "alice" || id.OrgID != store.DefaultOrg || id.Role != RoleMember || len(id.Teams) != 1 {
				t.Fatalf("identity = %+v", id)
			}
		})
	}
}

func TestAuthenticateRejects(t *testing.T) {
	keys := &fakeKeys{}
	disabled := setupKey(t, keys, "off", func(k *store.APIKey) { k.Disabled = true })
	expired := setupKey(t, keys, "old", func(k *store.APIKey) { k.ExpiresAt = testNow.Add(-time.Minute) })
	unknown, _, _, _ := NewKey()
	a := NewAuthenticator(keys, func() time.Time { return testNow })
	tests := map[string][2]string{
		"no header": {"", ""},
		"basic":     {"Authorization", "Basic eHl6"},
		"empty":     {"Authorization", "Bearer "},
		"unknown":   {"Authorization", "Bearer " + unknown},
		"disabled":  {"Authorization", "Bearer " + disabled},
		"expired":   {"x-api-key", expired},
	}
	for name, h := range tests {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", nil)
			if h[0] != "" {
				r.Header.Set(h[0], h[1])
			}
			if _, err := a.Authenticate(r); !errors.Is(err, ErrUnauthenticated) {
				t.Fatalf("err = %v, want ErrUnauthenticated", err)
			}
		})
	}
}
