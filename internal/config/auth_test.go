package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoadOIDC(t *testing.T) {
	c, err := Load(writeFile(t, `
auth:
  oidc:
    issuer: https://idp.example.com/realms/dev
    audience: transponder
    jwks_url: https://idp.example.com/keys
    user_claim: email
    teams_claim: teams
    roles_claim: realm_access.roles
    role_mapping:
      platform-admins: admin
      auditors: viewer
      staff: member
    clock_skew: 30s
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Auth.OIDC
	if !o.Enabled() || o.Issuer != "https://idp.example.com/realms/dev" || o.Audience != "transponder" || o.JWKSURL != "https://idp.example.com/keys" {
		t.Fatalf("oidc = %+v", o)
	}
	if o.UserClaim != "email" || o.TeamsClaim != "teams" || o.RolesClaim != "realm_access.roles" || o.ClockSkew != 30*time.Second {
		t.Fatalf("oidc claims = %+v", o)
	}
	if len(o.RoleMapping) != 3 || o.RoleMapping["platform-admins"] != "admin" || o.RoleMapping["auditors"] != "viewer" {
		t.Fatalf("role mapping = %+v", o.RoleMapping)
	}
}

func TestLoadOIDCDefaults(t *testing.T) {
	c, err := Load(writeFile(t, "auth:\n  oidc:\n    issuer: http://localhost:8081\n    audience: transponder\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	o := c.Auth.OIDC
	if o.UserClaim != "sub" || o.TeamsClaim != "groups" || o.RolesClaim != "" || o.ClockSkew != time.Minute {
		t.Fatalf("oidc defaults = %+v", o)
	}
	c, err = Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.OIDC.Enabled() || c.Auth.OIDC.UserClaim != "" {
		t.Fatalf("oidc is on without an issuer: %+v", c.Auth.OIDC)
	}
}

func TestLoadOIDCFromEnv(t *testing.T) {
	c, err := Load(writeFile(t, "auth:\n  oidc:\n    issuer: https://old.example.com\n    audience: old\n"), []string{
		"TRANSPONDER_AUTH__OIDC__ISSUER=https://idp.example.com",
		"TRANSPONDER_AUTH__OIDC__AUDIENCE=transponder",
		"TRANSPONDER_AUTH__OIDC__ROLES_CLAIM=roles",
		"TRANSPONDER_AUTH__OIDC__CLOCK_SKEW=2m",
		// A map cannot come from one variable, so this is ignored like any unknown name.
		"TRANSPONDER_AUTH__OIDC__ROLE_MAPPING=admins",
	})
	if err != nil {
		t.Fatal(err)
	}
	o := c.Auth.OIDC
	if o.Issuer != "https://idp.example.com" || o.Audience != "transponder" || o.RolesClaim != "roles" || o.ClockSkew != 2*time.Minute || len(o.RoleMapping) != 0 {
		t.Fatalf("oidc = %+v", o)
	}
}

func TestLoadOIDCRejects(t *testing.T) {
	tests := map[string]struct {
		yaml string
		want string
	}{
		"no audience":         {"issuer: https://idp.example.com", "auth.oidc.audience"},
		"no issuer":           {"audience: transponder", "auth.oidc.issuer"},
		"mapping, no issuer":  {"role_mapping: {a: admin}", "auth.oidc.issuer"},
		"relative issuer":     {"issuer: idp.example.com\n    audience: t", "auth.oidc.issuer"},
		"issuer not http":     {"issuer: ldap://idp.example.com\n    audience: t", "auth.oidc.issuer"},
		"bad jwks url":        {"issuer: https://idp.example.com\n    audience: t\n    jwks_url: /keys", "auth.oidc.jwks_url"},
		"mapping, no claim":   {"issuer: https://idp.example.com\n    audience: t\n    role_mapping: {a: admin}", "auth.oidc.roles_claim"},
		"unknown role":        {"issuer: https://idp.example.com\n    audience: t\n    roles_claim: r\n    role_mapping: {a: owner}", `"owner"`},
		"negative clock skew": {"issuer: https://idp.example.com\n    audience: t\n    clock_skew: -5s", "auth.oidc.clock_skew"},
		"unknown key":         {"issuer: https://idp.example.com\n    audience: t\n    client_secret: x", "client_secret"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeFile(t, "auth:\n  oidc:\n    "+tt.yaml+"\n"), nil)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to mention %s", err, tt.want)
			}
		})
	}
}
