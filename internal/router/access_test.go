package router

import (
	"reflect"
	"strings"
	"testing"

	"github.com/raven-clown/transponder/internal/auth"
	"github.com/raven-clown/transponder/internal/provider/echo"
)

func TestParseAccess(t *testing.T) {
	tests := map[string]struct {
		allow   []string
		wantErr string
	}{
		"empty":        {nil, ""},
		"every kind":   {[]string{"user:alice@example.com", "team:platform", "key:ci", "role:viewer"}, ""},
		"colon in id":  {[]string{"user:https://idp.example.com|42"}, ""},
		"no kind":      {[]string{"alice"}, "must be user:<id>"},
		"unknown kind": {[]string{"group:platform"}, "must be user:<id>"},
		"empty id":     {[]string{"team:"}, "must be user:<id>"},
		"unknown role": {[]string{"role:owner"}, "unknown role"},
		"empty role":   {[]string{"role:"}, "must be user:<id>"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a, err := ParseAccess(tt.allow)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if a.Open() != (len(tt.allow) == 0) || len(a.Entries()) != len(tt.allow) {
				t.Fatalf("access = %+v", a)
			}
		})
	}
}

func TestAccessPermits(t *testing.T) {
	alice := auth.Identity{User: "alice@example.com", Teams: []string{"ml", "platform"}, Role: auth.RoleMember}
	ciKey := auth.Identity{User: "ci", Key: "ci", Role: auth.RoleMember}
	auditor := auth.Identity{User: "vera", Role: auth.RoleViewer}
	root := auth.Identity{User: "root", Key: "root", Role: auth.RoleAdmin}
	nobody := auth.Identity{User: "bob", Role: auth.RoleMember}

	tests := map[string]struct {
		allow []string
		id    auth.Identity
		want  bool
	}{
		"open":                       {nil, nobody, true},
		"user":                       {[]string{"user:alice@example.com"}, alice, true},
		"other user":                 {[]string{"user:alice@example.com"}, nobody, false},
		"one of the teams":           {[]string{"team:platform"}, alice, true},
		"no team":                    {[]string{"team:platform"}, nobody, false},
		"key":                        {[]string{"key:ci"}, ciKey, true},
		"key name as a user":         {[]string{"user:ci"}, ciKey, true},
		"token user named like key":  {[]string{"key:ci"}, auth.Identity{User: "ci", Role: auth.RoleMember}, false},
		"role":                       {[]string{"role:viewer"}, auditor, true},
		"role above":                 {[]string{"role:viewer"}, root, true},
		"role below":                 {[]string{"role:viewer"}, nobody, false},
		"admin only":                 {[]string{"role:admin"}, auditor, false},
		"member role lets all in":    {[]string{"role:member"}, nobody, true},
		"any entry is enough":        {[]string{"user:x", "team:y", "key:ci"}, ciKey, true},
		"empty user matches nothing": {[]string{"user:x"}, auth.Identity{}, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			a, err := ParseAccess(tt.allow)
			if err != nil {
				t.Fatal(err)
			}
			if got := a.Permits(tt.id); got != tt.want {
				t.Fatalf("Permits(%+v) = %v, want %v", tt.id, got, tt.want)
			}
		})
	}
}

func TestAccessEntriesIsACopy(t *testing.T) {
	a, err := ParseAccess([]string{"team:platform"})
	if err != nil {
		t.Fatal(err)
	}
	a.Entries()[0] = "team:other"
	if got := a.Entries(); !reflect.DeepEqual(got, []string{"team:platform"}) {
		t.Fatalf("entries = %v", got)
	}
}

func TestPlanCarriesAccess(t *testing.T) {
	platform, err := ParseAccess([]string{"team:platform"})
	if err != nil {
		t.Fatal(err)
	}
	r := New([]Route{
		{Match: "fast", Access: platform, Targets: []Target{{Provider: echo.New("a"), Type: "echo"}}},
		{Match: "*", Targets: []Target{{Provider: echo.New("b"), Type: "echo"}}},
	})
	p, err := r.Plan("fast")
	if err != nil {
		t.Fatal(err)
	}
	if p.Access.Open() || !reflect.DeepEqual(p.Access.Entries(), []string{"team:platform"}) {
		t.Fatalf("plan access = %v", p.Access.Entries())
	}
	if p, _ = r.Plan("other"); !p.Access.Open() {
		t.Fatalf("open route has access %v", p.Access.Entries())
	}
}

func TestModelsFor(t *testing.T) {
	platform, err := ParseAccess([]string{"team:platform"})
	if err != nil {
		t.Fatal(err)
	}
	admins, err := ParseAccess([]string{"role:admin"})
	if err != nil {
		t.Fatal(err)
	}
	e := echo.New("e")
	r := New([]Route{
		{Match: "fast", Access: platform, Targets: []Target{{Provider: e, Type: "echo"}}},
		{Match: "big", Access: admins, Targets: []Target{{Provider: e, Type: "echo"}}},
		{Match: "small", Targets: []Target{{Provider: e, Type: "echo"}}},
		{Match: "*", Access: platform, Targets: []Target{{Provider: e, Type: "echo"}}},
	})
	tests := map[string]struct {
		id   auth.Identity
		want []string
	}{
		"member":         {auth.Identity{User: "bob", Role: auth.RoleMember}, []string{"small"}},
		"platform":       {auth.Identity{User: "alice", Teams: []string{"platform"}, Role: auth.RoleMember}, []string{"fast", "small"}},
		"admin":          {auth.Identity{User: "root", Role: auth.RoleAdmin}, []string{"big", "small"}},
		"platform admin": {auth.Identity{User: "ops", Teams: []string{"platform"}, Role: auth.RoleAdmin}, []string{"big", "fast", "small"}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := r.ModelsFor(tt.id); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ModelsFor = %v, want %v", got, tt.want)
			}
		})
	}
	if got := r.Models(); !reflect.DeepEqual(got, []string{"big", "fast", "small"}) {
		t.Fatalf("Models = %v", got)
	}
}
