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

func TestParseRole(t *testing.T) {
	tests := map[string]struct {
		in   string
		want Role
		ok   bool
	}{
		"admin":   {"admin", RoleAdmin, true},
		"member":  {"member", RoleMember, true},
		"viewer":  {"viewer", RoleViewer, true},
		"empty":   {"", RoleMember, true},
		"unknown": {"owner", "", false},
		"case":    {"Admin", "", false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := ParseRole(tt.in)
			if (err == nil) != tt.ok || got != tt.want {
				t.Fatalf("ParseRole(%q) = %q, %v", tt.in, got, err)
			}
		})
	}
}

func TestRoleAllows(t *testing.T) {
	tests := []struct {
		have, need Role
		want       bool
	}{
		{RoleAdmin, RoleAdmin, true},
		{RoleAdmin, RoleViewer, true},
		{RoleAdmin, RoleMember, true},
		{RoleViewer, RoleAdmin, false},
		{RoleViewer, RoleViewer, true},
		{RoleViewer, RoleMember, true},
		{RoleMember, RoleAdmin, false},
		{RoleMember, RoleViewer, false},
		{RoleMember, RoleMember, true},
		{"", RoleMember, false},
		{"owner", RoleMember, false},
		{RoleAdmin, "", false},
		{RoleAdmin, "owner", false},
	}
	for _, tt := range tests {
		t.Run(string(tt.have)+" needs "+string(tt.need), func(t *testing.T) {
			if got := tt.have.Allows(tt.need); got != tt.want {
				t.Fatalf("Allows = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIdentityRequire(t *testing.T) {
	if err := (Identity{Role: RoleViewer}).Require(RoleViewer); err != nil {
		t.Fatalf("viewer needs viewer: %v", err)
	}
	if err := (Identity{Role: RoleMember}).Require(RoleViewer); !errors.Is(err, ErrForbidden) {
		t.Fatalf("member needs viewer: err = %v, want ErrForbidden", err)
	}
}

func TestIdentitySubject(t *testing.T) {
	tests := map[string]struct {
		id   Identity
		want string
	}{
		"key":  {Identity{User: "ci", Key: "ci"}, "key:ci"},
		"user": {Identity{User: "alice@example.com"}, "user:alice@example.com"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tt.id.Subject(); got != tt.want {
				t.Fatalf("Subject = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIdentityContext(t *testing.T) {
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("an empty context has an identity")
	}
	want := Identity{OrgID: store.DefaultOrg, User: "alice", Teams: []string{"platform"}, Role: RoleViewer}
	got, ok := FromContext(WithIdentity(context.Background(), want))
	if !ok || got.User != want.User || got.Role != want.Role || len(got.Teams) != 1 {
		t.Fatalf("identity = %+v, %v", got, ok)
	}
}

func TestAuthenticateKeyRoles(t *testing.T) {
	tests := map[string]struct {
		stored string
		want   Role
	}{
		"admin":   {"admin", RoleAdmin},
		"viewer":  {"viewer", RoleViewer},
		"member":  {"member", RoleMember},
		"none":    {"", RoleMember},
		"unknown": {"owner", RoleMember},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			keys := &fakeKeys{}
			plain := setupKey(t, keys, "k", func(k *store.APIKey) { k.Role = tt.stored })
			a := NewAuthenticator(keys, func() time.Time { return testNow })
			r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			r.Header.Set("Authorization", "Bearer "+plain)
			id, err := a.Authenticate(r)
			if err != nil {
				t.Fatal(err)
			}
			if id.Role != tt.want {
				t.Fatalf("role = %q, want %q", id.Role, tt.want)
			}
		})
	}
}

type failingKeys struct{ fakeKeys }

var errStoreDown = errors.New("store down")

func (*failingKeys) KeyByHash(context.Context, [32]byte) (store.APIKey, error) {
	return store.APIKey{}, errStoreDown
}

func TestMiddleware(t *testing.T) {
	keys := &fakeKeys{}
	member := setupKey(t, keys, "bob", nil)
	viewer := setupKey(t, keys, "vera", func(k *store.APIKey) { k.Role = string(RoleViewer) })
	admin := setupKey(t, keys, "root", func(k *store.APIKey) { k.Role = string(RoleAdmin) })
	now := func() time.Time { return testNow }
	tests := map[string]struct {
		auth    *Authenticator
		token   string
		need    Role
		wantErr error
		wantID  string
	}{
		"no token":            {NewAuthenticator(keys, now), "", RoleMember, ErrUnauthenticated, ""},
		"member as member":    {NewAuthenticator(keys, now), member, RoleMember, nil, "bob"},
		"member needs viewer": {NewAuthenticator(keys, now), member, RoleViewer, ErrForbidden, ""},
		"viewer needs viewer": {NewAuthenticator(keys, now), viewer, RoleViewer, nil, "vera"},
		"viewer needs admin":  {NewAuthenticator(keys, now), viewer, RoleAdmin, ErrForbidden, ""},
		"admin needs admin":   {NewAuthenticator(keys, now), admin, RoleAdmin, nil, "root"},
		"store error":         {NewAuthenticator(&failingKeys{}, now), admin, RoleAdmin, errStoreDown, ""},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var denied error
			var seen Identity
			h := tt.auth.Middleware(tt.need, func(w http.ResponseWriter, _ *http.Request, err error) {
				denied = err
				w.WriteHeader(http.StatusTeapot)
			})(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				seen, _ = FromContext(r.Context())
			}))
			r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			if tt.token != "" {
				r.Header.Set("Authorization", "Bearer "+tt.token)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if !errors.Is(denied, tt.wantErr) {
				t.Fatalf("denied with %v, want %v", denied, tt.wantErr)
			}
			if seen.User != tt.wantID {
				t.Fatalf("next saw %+v, want user %q", seen, tt.wantID)
			}
			if (tt.wantErr != nil) != (w.Code == http.StatusTeapot) {
				t.Fatalf("status = %d", w.Code)
			}
		})
	}
}
